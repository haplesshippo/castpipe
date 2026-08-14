package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

type launchConn struct {
	read          *bytes.Reader
	writes        []message
	failTransport bool
}

func (c *launchConn) Read(p []byte) (int, error) { return c.read.Read(p) }
func (c *launchConn) Write(p []byte) (int, error) {
	m, err := readFrame(bytes.NewReader(p))
	if err != nil {
		return 0, err
	}
	if c.failTransport && m.Namespace == nsConnection && m.Dest != receiver {
		c.failTransport = false
		return 0, errors.New("transport connect failed")
	}
	c.writes = append(c.writes, m)
	return len(p), nil
}
func (c *launchConn) Close() error                     { return nil }
func (c *launchConn) LocalAddr() net.Addr              { return nil }
func (c *launchConn) RemoteAddr() net.Addr             { return nil }
func (c *launchConn) SetDeadline(time.Time) error      { return nil }
func (c *launchConn) SetReadDeadline(time.Time) error  { return nil }
func (c *launchConn) SetWriteDeadline(time.Time) error { return nil }

func TestLaunchRollsBackTransportFailure(t *testing.T) {
	status := message{Namespace: nsReceiver, Payload: `{"type":"RECEIVER_STATUS","status":{"applications":[{"appId":"CC1AD845","sessionId":"session-1","transportId":"transport-1"}]}}`}
	conn := &launchConn{read: bytes.NewReader(frame(status)), failTransport: true}
	s := &session{conn: conn}

	if _, _, err := s.launch(context.Background()); err == nil {
		t.Fatal("launch returned no error after transport CONNECT failed")
	}
	if len(conn.writes) != 2 || payloadType(conn.writes[1].Payload) != "STOP" {
		t.Fatalf("writes after failed launch were %+v, want LAUNCH followed by STOP", conn.writes)
	}
}

var connectMessage = message{
	Source:    sender,
	Dest:      receiver,
	Namespace: nsConnection,
	Payload:   `{"type":"CONNECT"}`,
}

// TestEncodeExactBytes pins the wire format down byte for byte. A wrong field
// number or wire type still round-trips through our own decoder, so a
// round-trip test alone would pass while every device silently ignored us.
func TestEncodeExactBytes(t *testing.T) {
	var want []byte
	want = append(want, 0x08, 0x00) // field 1 varint: protocol_version = 0
	want = append(want, 0x12, byte(len(sender)))
	want = append(want, sender...) // field 2 string: source_id
	want = append(want, 0x1A, byte(len(receiver)))
	want = append(want, receiver...) // field 3 string: destination_id
	want = append(want, 0x22, byte(len(nsConnection)))
	want = append(want, nsConnection...) // field 4 string: namespace
	want = append(want, 0x28, 0x00)      // field 5 varint: payload_type = STRING
	want = append(want, 0x32, byte(len(connectMessage.Payload)))
	want = append(want, connectMessage.Payload...) // field 6 string: payload_utf8

	if got := encode(connectMessage); !bytes.Equal(got, want) {
		t.Errorf("encode produced the wrong bytes\n got %x\nwant %x", got, want)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	for _, m := range []message{
		connectMessage,
		{sender, "transport-42", nsMedia, `{"type":"LOAD","requestId":3}`},
		// Non-ASCII survives because payloads are length-delimited bytes, not
		// C strings; track titles are full of it.
		{sender, receiver, nsReceiver, `{"title":"Sinnerman – Nina Simone"}`},
		{}, // every field empty
	} {
		got, err := decode(encode(m))
		if err != nil {
			t.Fatalf("decode(encode(%+v)): %v", m, err)
		}
		if got != m {
			t.Errorf("round trip changed the message\n got %+v\nwant %+v", got, m)
		}
	}
}

// TestVarintLengthsAcrossBoundary checks payloads either side of 127 bytes,
// where the length prefix stops fitting in a single varint byte.
func TestVarintLengthsAcrossBoundary(t *testing.T) {
	for _, size := range []int{0, 1, 126, 127, 128, 129, 300, 16384} {
		m := message{Namespace: nsMedia, Payload: strings.Repeat("x", size)}
		got, err := decode(encode(m))
		if err != nil {
			t.Fatalf("payload of %d bytes: %v", size, err)
		}
		if len(got.Payload) != size {
			t.Errorf("payload of %d bytes came back as %d", size, len(got.Payload))
		}
	}
}

func TestFrameCarriesBigEndianLength(t *testing.T) {
	f := frame(connectMessage)
	body := encode(connectMessage)
	if got := binary.BigEndian.Uint32(f[:4]); int(got) != len(body) {
		t.Errorf("frame length prefix = %d, want %d", got, len(body))
	}
	if !bytes.Equal(f[4:], body) {
		t.Error("frame body does not match the encoded message")
	}
}

func TestReadFrame(t *testing.T) {
	// Two frames back to back: the reader must consume exactly one.
	stream := bytes.NewReader(append(frame(connectMessage), frame(connectMessage)...))
	for i := range 2 {
		got, err := readFrame(stream)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if got != connectMessage {
			t.Errorf("frame %d = %+v, want %+v", i, got, connectMessage)
		}
	}
	if _, err := readFrame(stream); !errors.Is(err, io.EOF) {
		t.Errorf("reading past the end gave %v, want EOF", err)
	}
}

func TestReadFrameRejectsAbsurdLength(t *testing.T) {
	// A length prefix we trusted would have us allocate whatever a device -
	// or anything else that connected - claimed to be sending.
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], maxFrame+1)
	if _, err := readFrame(bytes.NewReader(header[:])); !errors.Is(err, errProtobuf) {
		t.Errorf("oversized frame gave %v, want errProtobuf", err)
	}
}

func TestReadFrameRejectsTruncatedBody(t *testing.T) {
	f := frame(connectMessage)
	if _, err := readFrame(bytes.NewReader(f[:len(f)-3])); err == nil {
		t.Error("a truncated frame body should be an error")
	}
}

// TestDecodeSkipsUnknownFields matters because the receiver is free to add
// fields: an unknown one must be stepped over, not treated as corruption.
func TestDecodeSkipsUnknownFields(t *testing.T) {
	b := encode(connectMessage)
	b = append(b, 0x7D, 0x01, 0x02, 0x03, 0x04) // field 15, fixed32
	b = append(b, 0x78, 0x96, 0x01)             // field 15, varint
	b = append(b, 0x79, 1, 2, 3, 4, 5, 6, 7, 8) // field 15, fixed64
	b = append(b, 0x7A, 0x02, 'h', 'i')         // field 15, length-delimited

	got, err := decode(b)
	if err != nil {
		t.Fatalf("decode with unknown fields: %v", err)
	}
	if got != connectMessage {
		t.Errorf("unknown fields changed the message: %+v", got)
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	good := encode(connectMessage)
	cases := map[string][]byte{
		"length runs past the end": {0x12, 0x7F, 'a'},
		"truncated varint":         {0x08},
		"unused wire type 6":       {0x0E, 0x01},
		"unused wire type 7":       {0x0F, 0x01},
		"truncated fixed32":        {0x0D, 0x01},
		"truncated fixed64":        {0x09, 0x01},
		"cut mid-message":          good[:len(good)-4],
	}
	for name, b := range cases {
		if _, err := decode(b); err == nil {
			t.Errorf("%s: decode returned no error", name)
		}
	}
}

func TestPayloadType(t *testing.T) {
	for payload, want := range map[string]string{
		`{"type":"PING"}`:                       "PING",
		`{"requestId":1,"type":"MEDIA_STATUS"}`: "MEDIA_STATUS",
		`{}`:                                    "",
		`not json at all`:                       "",
	} {
		if got := payloadType(payload); got != want {
			t.Errorf("payloadType(%s) = %q, want %q", payload, got, want)
		}
	}
}

// TestParseMedia covers the shapes the receiver actually sends: a status while
// playing, and the terminal one that tells us the track is over.
func TestParseMedia(t *testing.T) {
	playing := `{"type":"MEDIA_STATUS","status":[{"mediaSessionId":1,
		"playerState":"PLAYING","currentTime":19.5,"media":{"duration":185.7}}]}`
	states := parseMedia(playing)
	if len(states) != 1 {
		t.Fatalf("got %d states, want 1", len(states))
	}
	if states[0].PlayerState != "PLAYING" || states[0].CurrentTime != 19.5 {
		t.Errorf("playing status parsed as %+v", states[0])
	}
	if states[0].Media.Duration != 185.7 {
		t.Errorf("duration = %v, want 185.7", states[0].Media.Duration)
	}

	// An empty status array is what arrives once the receiver goes idle, and
	// it must not be mistaken for a track that is still running.
	if got := parseMedia(`{"type":"MEDIA_STATUS","status":[]}`); len(got) != 0 {
		t.Errorf("idle status parsed as %d states", len(got))
	}
	if got := parseMedia(`garbage`); got != nil {
		t.Errorf("garbage parsed as %+v", got)
	}
}
