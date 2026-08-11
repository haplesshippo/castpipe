package main

// Serving the file. The device does not read our disk: it fetches the media
// over HTTP from us, so `play` has to stay alive for as long as it is playing.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// contentTypes is deliberately a list of what the Default Media Receiver can
// actually decode. Anything else is refused up front instead of being handed
// over in the hope that it works; notably .mkv, which the receiver does not
// support however common it is.
var contentTypes = map[string]string{
	".aac":  "audio/aac",
	".flac": "audio/flac",
	".m4a":  "audio/mp4",
	".mp3":  "audio/mpeg",
	".oga":  "audio/ogg",
	".ogg":  "audio/ogg",
	".opus": "audio/ogg",
	".wav":  "audio/wav",
	".m4v":  "video/mp4",
	".mp4":  "video/mp4",
	".webm": "video/webm",
}

func contentType(path string) (string, error) {
	if t, ok := contentTypes[strings.ToLower(filepath.Ext(path))]; ok {
		return t, nil
	}
	kinds := slices.Sorted(maps.Keys(contentTypes))
	return "", fmt.Errorf("a Chromecast cannot play %s files; supported: %s",
		strings.ToLower(filepath.Ext(path)), strings.Join(kinds, " "))
}

// fileServer serves exactly one file, at one unguessable path, to whoever can
// reach it. http.ServeFile gives us correct byte-range handling, which the
// device relies on from its very first request.
type fileServer struct {
	listener net.Listener
	url      string
	requests atomic.Int64
}

func serveFile(path, mediaType, deviceHost string) (*fileServer, error) {
	local, err := localAddressFor(deviceHost)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, err
	}
	token := make([]byte, 8)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	// Two spellings of the same path. The device is given the escaped one,
	// but net/http hands the handler the decoded one, so a track called
	// "16 here comes the sun.mp3" arrives with real spaces and would never
	// match the %20 form.
	route := "/" + hex.EncodeToString(token) + "/" + filepath.Base(path)
	escaped := "/" + hex.EncodeToString(token) + "/" + url.PathEscape(filepath.Base(path))

	s := &fileServer{listener: listener}
	port := listener.Addr().(*net.TCPAddr).Port
	s.url = "http://" + net.JoinHostPort(local, strconv.Itoa(port)) + escaped

	server := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		// The receiver is a web page, so the media is a cross-origin
		// subresource and needs the header to match.
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != route {
				http.NotFound(w, r)
				return
			}
			s.requests.Add(1)
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Content-Type", mediaType)
			http.ServeFile(w, r, path)
		}),
		// The device resets connections instead of closing them; there is
		// nothing useful to say about that on stderr.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	go func() { _ = server.Serve(listener) }()
	return s, nil
}

func (s *fileServer) Close() error { return s.listener.Close() }

// Fetched reports whether the device ever actually asked us for the bytes.
// Playing state without a fetch would mean it is playing something else.
func (s *fileServer) Fetched() bool { return s.requests.Load() > 0 }

// localAddressFor picks the address of ours that can reach the device. On a
// machine with a VPN, several interfaces or a container bridge, the obvious
// answer is often wrong; asking the routing table which source it would use is
// the answer that matches what the device will actually see. Connecting a UDP
// socket sends nothing.
func localAddressFor(host string) (string, error) {
	conn, err := net.Dial("udp", net.JoinHostPort(host, "9"))
	if err != nil {
		return "", fmt.Errorf("no route from this machine to %s: %w", host, err)
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

// localNetworkHint replaces the error macOS gives an application that has not
// been granted local network access. It reports EHOSTUNREACH, which reads as a
// routing fault and sends people to debug their router instead.
func localNetworkHint(err error) error {
	if runtime.GOOS != "darwin" || !errors.Is(err, syscall.EHOSTUNREACH) {
		return err
	}
	return fmt.Errorf("%w\n\n"+
		"That is almost certainly macOS Local Network privacy, not your network:\n"+
		"macOS reports a blocked local connection as a routing error. Allow\n"+
		"castpipe under System Settings > Privacy & Security > Local Network,\n"+
		"then run it again.", err)
}

func isReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}
