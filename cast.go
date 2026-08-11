package main

// castv2: length-prefixed protobuf frames over TLS carrying JSON payloads on a
// few namespaces. Exactly one message type exists on the wire, so rather than
// depend on a generated package we encode its six fields directly:
//
//	message CastMessage {
//	  ProtocolVersion protocol_version = 1;  // 0 = CASTV2_1_0
//	  string source_id = 2; string destination_id = 3; string namespace = 4;
//	  PayloadType payload_type = 5;          // 0 = STRING
//	  string payload_utf8 = 6;
//	}

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	nsConnection = "urn:x-cast:com.google.cast.tp.connection"
	nsHeartbeat  = "urn:x-cast:com.google.cast.tp.heartbeat"
	nsReceiver   = "urn:x-cast:com.google.cast.receiver"
	nsMedia      = "urn:x-cast:com.google.cast.media"

	// Google's Default Media Receiver plays plain media URLs and needs no
	// developer registration or fee.
	defaultReceiver = "CC1AD845"

	sender   = "sender-0"
	receiver = "receiver-0"
	maxFrame = 1 << 20
)

var errProtobuf = errors.New("malformed castv2 message")

type message struct{ Source, Dest, Namespace, Payload string }

func encode(m message) []byte {
	var b []byte
	b = putVarint(b, 1, 0) // protocol_version
	b = putString(b, 2, m.Source)
	b = putString(b, 3, m.Dest)
	b = putString(b, 4, m.Namespace)
	b = putVarint(b, 5, 0) // payload_type STRING
	return putString(b, 6, m.Payload)
}

func decode(b []byte) (message, error) {
	var m message
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return m, errProtobuf
		}
		b = b[n:]
		switch field, wire := key>>3, key&7; wire {
		case 0: // varint
			_, n := binary.Uvarint(b)
			if n <= 0 {
				return m, errProtobuf
			}
			b = b[n:]
		case 2: // length-delimited
			size, n := binary.Uvarint(b)
			if n <= 0 || size > uint64(len(b)-n) {
				return m, errProtobuf
			}
			value := string(b[n : n+int(size)])
			b = b[n+int(size):]
			switch field {
			case 2:
				m.Source = value
			case 3:
				m.Dest = value
			case 4:
				m.Namespace = value
			case 6:
				m.Payload = value
			}
		case 5: // fixed32
			if len(b) < 4 {
				return m, errProtobuf
			}
			b = b[4:]
		case 1: // fixed64
			if len(b) < 8 {
				return m, errProtobuf
			}
			b = b[8:]
		default:
			return m, errProtobuf
		}
	}
	return m, nil
}

func base128(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func putVarint(b []byte, field int, v uint64) []byte {
	return base128(base128(b, uint64(field)<<3), v)
}

func putString(b []byte, field int, s string) []byte {
	b = base128(base128(b, uint64(field)<<3|2), uint64(len(s)))
	return append(b, s...)
}

func frame(m message) []byte {
	body := encode(m)
	out := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(out, uint32(len(body)))
	return append(out, body...)
}

func readFrame(r io.Reader) (message, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return message{}, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > maxFrame {
		return message{}, fmt.Errorf("%w: frame of %d bytes", errProtobuf, size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return message{}, err
	}
	return decode(body)
}

// session is one castv2 connection. Writes are serialised because the
// interrupt handler sends on it while the main flow is blocked reading.
type session struct {
	conn      net.Conn
	mu        sync.Mutex
	requestID int
}

// connect opens the TLS connection and joins the receiver's virtual
// connection, without which the device ignores everything else we send.
func connect(d Device) (*session, error) {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: dialTimeout}, "tcp", d.Addr(),
		// Cast devices present a self-signed certificate for a name that
		// resolves to nothing. There is nothing to verify it against, and the
		// channel carries no secrets.
		&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // see above
	if err != nil {
		return nil, localNetworkHint(fmt.Errorf("connecting to %s: %w", d.Addr(), err))
	}
	s := &session{conn: conn}
	if err := s.send(nsConnection, receiver, payload{"type": "CONNECT"}); err != nil {
		conn.Close()
		return nil, err
	}
	return s, nil
}

func (s *session) Close() error { return s.conn.Close() }

type payload map[string]any

func (s *session) send(namespace, dest string, p payload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.conn.Write(frame(message{sender, dest, namespace, string(body)}))
	return err
}

func (s *session) nextRequest() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestID++
	return s.requestID
}

// wait reads until handle reports it has seen what it was waiting for,
// answering the device's heartbeat along the way: a connection whose PING goes
// unanswered is dropped after a few seconds. A zero deadline means no limit.
func (s *session) wait(deadline time.Time, handle func(message) (bool, error)) error {
	if err := s.conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	for {
		m, err := readFrame(s.conn)
		if err != nil {
			return err
		}
		if m.Namespace == nsHeartbeat {
			if payloadType(m.Payload) == "PING" {
				if err := s.send(nsHeartbeat, m.Source, payload{"type": "PONG"}); err != nil {
					return err
				}
			}
			continue
		}
		if done, err := handle(m); err != nil || done {
			return err
		}
	}
}

func payloadType(p string) string {
	var v struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(p), &v)
	return v.Type
}

type receiverStatus struct {
	Status struct {
		Applications []struct {
			AppID       string `json:"appId"`
			SessionID   string `json:"sessionId"`
			TransportID string `json:"transportId"`
		} `json:"applications"`
	} `json:"status"`
}

// receiverApp waits for the device to report its running applications and
// returns the one with appID, or with appID "" whichever app is running. An
// empty sessionID means the device is idle.
func (s *session) receiverApp(appID string) (transport, sessionID string, err error) {
	err = s.wait(time.Now().Add(replyTimeout), func(m message) (bool, error) {
		if m.Namespace != nsReceiver {
			return false, nil
		}
		switch payloadType(m.Payload) {
		case "LAUNCH_ERROR":
			return false, fmt.Errorf("the device would not start the media receiver: %s", m.Payload)
		case "RECEIVER_STATUS":
			var status receiverStatus
			if err := json.Unmarshal([]byte(m.Payload), &status); err != nil {
				return false, err
			}
			for _, app := range status.Status.Applications {
				if appID == "" || app.AppID == appID {
					transport, sessionID = app.TransportID, app.SessionID
					return true, nil
				}
			}
			return appID == "", nil // idle, and idle is the answer to "what is running?"
		}
		return false, nil
	})
	return transport, sessionID, err
}

// launch starts the Default Media Receiver and opens a virtual connection to
// it, which it needs before it will accept a LOAD.
func (s *session) launch(ctx context.Context) (transport, sessionID string, err error) {
	p := payload{"type": "LAUNCH", "requestId": s.nextRequest(), "appId": defaultReceiver}
	if err := s.send(nsReceiver, receiver, p); err != nil {
		return "", "", err
	}
	if transport, sessionID, err = s.receiverApp(defaultReceiver); err != nil {
		s.rollbackLaunch("")
		return "", "", err
	}
	if err := ctx.Err(); err != nil {
		s.rollbackLaunch(sessionID)
		return "", "", err
	}
	if err := s.send(nsConnection, transport, payload{"type": "CONNECT"}); err != nil {
		s.rollbackLaunch(sessionID)
		return "", "", err
	}
	if err := ctx.Err(); err != nil {
		s.rollbackLaunch(sessionID)
		return "", "", err
	}
	return transport, sessionID, nil
}

func (s *session) rollbackLaunch(sessionID string) {
	if sessionID == "" {
		var err error
		sessionID, err = s.running()
		if err != nil || sessionID == "" {
			return
		}
	}
	_ = s.quit(sessionID)
}

// running reports the session id of whatever the device is playing, or "".
func (s *session) running() (string, error) {
	p := payload{"type": "GET_STATUS", "requestId": s.nextRequest()}
	if err := s.send(nsReceiver, receiver, p); err != nil {
		return "", err
	}
	_, sessionID, err := s.receiverApp("")
	return sessionID, err
}

// quit stops the running receiver application, returning the device to idle.
func (s *session) quit(sessionID string) error {
	return s.send(nsReceiver, receiver, payload{
		"type": "STOP", "requestId": s.nextRequest(), "sessionId": sessionID,
	})
}

type playerState struct {
	PlayerState string  `json:"playerState"`
	CurrentTime float64 `json:"currentTime"`
	IdleReason  string  `json:"idleReason"`
	Media       struct {
		Duration float64 `json:"duration"`
	} `json:"media"`
}

func parseMedia(p string) []playerState {
	var status struct {
		Status []playerState `json:"status"`
	}
	_ = json.Unmarshal([]byte(p), &status)
	return status.Status
}

// load hands the device a URL and waits until it is really playing. A device
// that cannot decode the file says so, and that is reported rather than
// smoothed over.
func (s *session) load(transport, url, mediaType, title string) (duration float64, err error) {
	p := payload{
		"type": "LOAD", "requestId": s.nextRequest(), "autoplay": true, "currentTime": 0,
		"media": payload{
			"contentId": url, "contentType": mediaType, "streamType": "BUFFERED",
			"metadata": payload{"metadataType": 0, "title": title},
		},
	}
	if err := s.send(nsMedia, transport, p); err != nil {
		return 0, err
	}
	err = s.wait(time.Now().Add(loadTimeout), func(m message) (bool, error) {
		if m.Namespace != nsMedia {
			return false, nil
		}
		switch payloadType(m.Payload) {
		case "LOAD_FAILED", "LOAD_CANCELLED", "INVALID_REQUEST", "ERROR":
			return false, fmt.Errorf("the device would not play this file: %s", m.Payload)
		case "MEDIA_STATUS":
			for _, st := range parseMedia(m.Payload) {
				if st.IdleReason == "ERROR" {
					return false, errors.New("the device could not decode this file")
				}
				if st.PlayerState == "PLAYING" {
					duration = st.Media.Duration
					return true, nil
				}
			}
		}
		return false, nil
	})
	return duration, err
}

// errTakenOver means the device stopped being ours: a phone cast to it, or
// something ran `castpipe stop`. It is a normal way for playback to end.
var errTakenOver = errors.New("playback was stopped on the device")

// follow blocks until the track ends or the device is taken away. The device
// volunteers a status when the player changes state, which is how the end of
// the track arrives; it does not stream positions and castpipe does not poll.
func (s *session) follow(sessionID string) error {
	err := s.wait(time.Time{}, func(m message) (bool, error) {
		switch m.Namespace {
		case nsMedia:
			for _, st := range parseMedia(m.Payload) {
				if st.PlayerState == "IDLE" && st.IdleReason != "" {
					return true, nil
				}
			}
		case nsReceiver:
			// Our session vanishing from the device's status is the only
			// notice we get; without this the command would wait for a track
			// that has already been replaced, serving a file nobody reads.
			var status receiverStatus
			if json.Unmarshal([]byte(m.Payload), &status) != nil {
				return false, nil
			}
			for _, app := range status.Status.Applications {
				if app.SessionID == sessionID {
					return false, nil
				}
			}
			return false, errTakenOver
		}
		return false, nil
	})
	// The device resets the connection rather than closing it politely, so an
	// abrupt end is not worth failing the command over.
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || isReset(err) {
		return nil
	}
	return err
}
