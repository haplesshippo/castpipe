package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		command string
		rest    []string
		to      string
	}{
		{[]string{"devices"}, "devices", nil, ""},
		{[]string{"play", "a.mp3"}, "play", []string{"a.mp3"}, ""},
		// --to must work on either side of the file name.
		{[]string{"play", "a.mp3", "--to", "TV"}, "play", []string{"a.mp3"}, "TV"},
		{[]string{"play", "--to", "TV", "a.mp3"}, "play", []string{"a.mp3"}, "TV"},
		{[]string{"play", "--to=TV", "a.mp3"}, "play", []string{"a.mp3"}, "TV"},
		{[]string{"stop", "--to", "Living Room Speakers"}, "stop", nil, "Living Room Speakers"},
		// A file whose name starts with a dash is still just a file.
		{[]string{"play", "--to", "TV", "-odd.mp3"}, "play", []string{"-odd.mp3"}, "TV"},
	} {
		command, rest, to, err := parseArgs(tc.args)
		if err != nil {
			t.Errorf("%v: %v", tc.args, err)
			continue
		}
		if command != tc.command || to != tc.to || !slices.Equal(rest, tc.rest) {
			t.Errorf("parseArgs(%v) = %q, %v, %q; want %q, %v, %q",
				tc.args, command, rest, to, tc.command, tc.rest, tc.to)
		}
	}
}

func TestParseArgsRejectsDanglingTo(t *testing.T) {
	if _, _, _, err := parseArgs([]string{"play", "a.mp3", "--to"}); err == nil {
		t.Error("--to with nothing after it should be an error")
	}
}

func TestRunRejectsUnknownAndIncomplete(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"launch", "a.mp3"},
		{"play"},                   // no file
		{"play", "a.mp3", "b.mp3"}, // two files
		{"play", "a.mp3", "-t", "TV"},
		{"devices", "extra"},
		{"devices", "--to", "TV"},
		{"stop", "extra"},
	} {
		if err := run(args); err == nil {
			t.Errorf("run(%v) returned no error", args)
		}
	}
}

// TestPlayRefusesUnplayable checks that a real file the receiver cannot play
// fails on the format, immediately, rather than after a discovery timeout or,
// worse, silently on the device.
func TestPlayRefusesUnplayable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "film.mkv")
	if err := os.WriteFile(path, []byte("fake matroska"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := play(path, "10.0.0.1:8009")
	if err == nil {
		t.Fatal("playing an .mkv returned no error")
	}
	if !strings.Contains(err.Error(), "cannot play") {
		t.Errorf("error was %v, want it to name the format problem", err)
	}
}

func TestPlayReportsMissingFile(t *testing.T) {
	err := play("/nonexistent/track.mp3", "10.0.0.1:8009")
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("error was %v, want it to say the file is missing", err)
	}
}

// TestResolveDirectAddress covers the escape hatch for networks where mDNS
// does not get through: --to may be a literal host:port. It must not start a
// discovery, which is also what keeps this test off the network.
func TestResolveDirectAddress(t *testing.T) {
	for _, tc := range []struct {
		to   string
		host string
		port int
	}{
		{"192.168.1.113:32064", "192.168.1.113", 32064},
		{"192.168.1.217:8009", "192.168.1.217", 8009},
		{"chromecast.local:8009", "chromecast.local", 8009},
	} {
		device, err := resolve(tc.to)
		if err != nil {
			t.Errorf("resolve(%q): %v", tc.to, err)
			continue
		}
		if device.Host != tc.host || device.Port != tc.port {
			t.Errorf("resolve(%q) = %s:%d, want %s:%d",
				tc.to, device.Host, device.Port, tc.host, tc.port)
		}
	}
}

func TestResolveRejectsBadPort(t *testing.T) {
	if _, err := resolve("192.168.1.113:notaport"); err == nil {
		t.Error("a non-numeric port should be an error, not a device name")
	}
}

func TestClock(t *testing.T) {
	for seconds, want := range map[float64]string{
		0: "0:00", 5: "0:05", 59.9: "0:59", 60: "1:00", 185.7: "3:05", 3600: "60:00",
	} {
		if got := clock(seconds); got != want {
			t.Errorf("clock(%v) = %q, want %q", seconds, got, want)
		}
	}
	if got := bracket(0); got != "" {
		t.Errorf("bracket(0) = %q, want empty for an unknown duration", got)
	}
	if got := bracket(185.7); got != " (3:05)" {
		t.Errorf("bracket(185.7) = %q", got)
	}
}

func TestNames(t *testing.T) {
	got := names([]Device{{Name: "TV"}, {Name: "Living Room Speakers"}})
	if got != "  TV\n  Living Room Speakers" {
		t.Errorf("names() = %q", got)
	}
}
