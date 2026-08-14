package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContentType(t *testing.T) {
	for path, want := range map[string]string{
		"/music/track.mp3":   "audio/mpeg",
		"/music/TRACK.MP3":   "audio/mpeg",
		"/music/track.flac":  "audio/flac",
		"a.b.c/odd name.m4a": "audio/mp4",
		"/video/clip.mp4":    "video/mp4",
		"/video/clip.webm":   "video/webm",
	} {
		got, err := contentType(path)
		if err != nil {
			t.Errorf("contentType(%q): %v", path, err)
		} else if got != want {
			t.Errorf("contentType(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestContentTypeRefusesUnplayable is the "honest failure" path: a Chromecast
// cannot play these, so castpipe says so rather than handing the device a URL
// and leaving the user to wonder why nothing happened.
func TestContentTypeRefusesUnplayable(t *testing.T) {
	for _, path := range []string{"film.mkv", "song.wma", "book.pdf", "noextension"} {
		if _, err := contentType(path); err == nil {
			t.Errorf("contentType(%q) accepted a format the receiver cannot play", path)
		} else if !strings.Contains(err.Error(), ".mp3") {
			t.Errorf("contentType(%q) error does not list what is supported: %v", path, err)
		}
	}
}

// TestServeFileRanges is the behaviour the device depends on. Its very first
// request carries a Range header, and seeking is done by asking for a byte
// offset, so a server that answered 200 with the whole body would appear to
// work and then fail.
func TestServeFileRanges(t *testing.T) {
	body := strings.Repeat("0123456789", 100) // 1000 bytes
	path := filepath.Join(t.TempDir(), "track.mp3")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := serveFile(path, "audio/mpeg", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	if server.Fetched() {
		t.Error("Fetched() is true before the device has asked for anything")
	}

	// The open-ended range the device opens with.
	resp := get(t, server.url, "bytes=0-")
	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("open-ended range: status %d, want 206", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 0-999/1000" {
		t.Errorf("Content-Range = %q, want %q", got, "bytes 0-999/1000")
	}
	if got := resp.Header.Get("Content-Type"); got != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want audio/mpeg", got)
	}
	// The receiver is a web page, so the media is a cross-origin subresource.
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}

	// The mid-file range a seek produces.
	resp = get(t, server.url, "bytes=500-599")
	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("mid-file range: status %d, want 206", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if string(got) != body[500:600] {
		t.Errorf("mid-file range returned the wrong 100 bytes: %q", got)
	}

	if !server.Fetched() {
		t.Error("Fetched() is false after the file was fetched twice")
	}
}

func TestServeFileWholeBodyAndUnknownPath(t *testing.T) {
	body := "not really an mp3"
	path := filepath.Join(t.TempDir(), "track.mp3")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := serveFile(path, "audio/mpeg", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	resp := get(t, server.url, "")
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != body {
		t.Errorf("plain GET = %d %q, want 200 %q", resp.StatusCode, got, body)
	}

	// Only the one unguessable path serves the file; the server is reachable
	// by anything on the network for as long as playback lasts.
	base := server.url[:strings.LastIndex(server.url, "/")]
	resp = get(t, base+"/../../etc/passwd", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unrelated path = %d, want 404", resp.StatusCode)
	}
}

// TestServeFileNameWithSpaces is a regression test for a real failure against
// hardware: the URL is handed to the device escaped, but net/http gives the
// handler the decoded path, so matching the two spellings against each other
// 404'd every track whose name contained a space - which is most of them.
func TestServeFileNameWithSpaces(t *testing.T) {
	body := "pretend this is audio"
	path := filepath.Join(t.TempDir(), "16 here comes the sun.mp3")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := serveFile(path, "audio/mpeg", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	// The device fetches this URL, so it must carry an address rather than a
	// name only we can resolve, and it must be a legal URL.
	if !strings.HasPrefix(server.url, "http://127.0.0.1:") {
		t.Errorf("url = %q, want it to start with the local address", server.url)
	}
	if strings.Contains(server.url, " ") {
		t.Errorf("url = %q contains an unescaped space", server.url)
	}
	if !strings.Contains(server.url, "16%20here%20comes%20the%20sun.mp3") {
		t.Errorf("url = %q does not keep the file name", server.url)
	}

	// The part that actually broke: fetching that exact URL must return the file.
	resp := get(t, server.url, "")
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fetching the URL we hand the device gave %d, want 200", resp.StatusCode)
	}
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}

func get(t *testing.T, url, rangeHeader string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
