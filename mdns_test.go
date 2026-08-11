package main

import (
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

// TestParseReplies parses whole _googlecast._tcp replies, laid out the way a
// Chromecast lays them out. It is the test that matters most: it proves the
// parser can follow compression pointers, join records that arrive in
// different sections and different packets, and above all that a cast group's
// port comes from its SRV record rather than being assumed to be 8009 - the
// mistake that silently casts to one speaker instead of to the whole group.
//
// See testdata/gen.go for how the fixture is built and why.
func TestParseReplies(t *testing.T) {
	r := newRecords()
	for _, packet := range capturedPackets(t) {
		if err := r.parse(packet); err != nil {
			t.Fatalf("parsing a reply: %v", err)
		}
	}
	devices := r.devices()
	if len(devices) != 4 {
		t.Fatalf("got %d devices, want 4:\n%s", len(devices), names(devices))
	}

	byName := map[string]Device{}
	for _, d := range devices {
		if d.Host == "" || d.Port == 0 || d.Name == "" {
			t.Errorf("incomplete device: %+v", d)
		}
		byName[d.Name] = d
	}

	for _, want := range []Device{
		{"Example Speaker Group", "Google Cast Group", "192.0.2.11", 32001},
		{"Example Group Two", "Google Cast Group", "192.0.2.12", 32002},
		{"Example Media Player", "Chromecast Ultra", "192.0.2.12", 8009},
		{"Example Speaker", "Google Home", "192.0.2.11", 8009},
	} {
		got, ok := byName[want.Name]
		if !ok {
			t.Errorf("no device named %q; found:\n%s", want.Name, names(devices))
			continue
		}
		if got != want {
			t.Errorf("device %q = %+v, want %+v", want.Name, got, want)
		}
	}

	// The groups and the single devices together are what show the port is
	// genuinely read per device rather than defaulted to 8009.
	if a, b := byName["Example Speaker Group"], byName["Example Group Two"]; a.Port == b.Port {
		t.Errorf("both groups parsed to port %d; they are on different ports", a.Port)
	}

	// A group runs on a port of a device that is also listed in its own right,
	// so the same address must be able to carry two entries on two ports.
	if group, speaker := byName["Example Speaker Group"], byName["Example Speaker"]; group.Host != speaker.Host {
		t.Errorf("group host %s and speaker host %s should be the same machine",
			group.Host, speaker.Host)
	}
}

// TestParseIgnoresForeignServiceInReplies checks the fixture's non-Cast reply
// does not become a device, since anything at all may answer on 5353.
func TestParseIgnoresForeignServiceInReplies(t *testing.T) {
	r := newRecords()
	for _, packet := range capturedPackets(t) {
		_ = r.parse(packet)
	}
	for _, d := range r.devices() {
		if strings.Contains(strings.ToLower(d.Name), "printer") {
			t.Errorf("a _printer._tcp reply was parsed as a Cast device: %+v", d)
		}
	}
}

// capturedPackets reads testdata/mdns-replies.bin: each packet stored as a
// 4-byte big-endian length followed by that many bytes.
func capturedPackets(t *testing.T) [][]byte {
	t.Helper()
	blob, err := os.ReadFile("testdata/mdns-replies.bin")
	if err != nil {
		t.Fatal(err)
	}
	var packets [][]byte
	for off := 0; off+4 <= len(blob); {
		size := int(binary.BigEndian.Uint32(blob[off : off+4]))
		if off+4+size > len(blob) {
			t.Fatalf("truncated capture at offset %d", off)
		}
		packets = append(packets, blob[off+4:off+4+size])
		off += 4 + size
	}
	return packets
}

func TestQueryIsAWellFormedQuestion(t *testing.T) {
	q := query()
	if got := binary.BigEndian.Uint16(q[4:6]); got != 1 {
		t.Errorf("question count = %d, want 1", got)
	}
	name, next, err := readName(q, 12)
	if err != nil {
		t.Fatal(err)
	}
	if name != castService {
		t.Errorf("question name = %q, want %q", name, castService)
	}
	if got := binary.BigEndian.Uint16(q[next : next+2]); got != typePTR {
		t.Errorf("question type = %d, want PTR", got)
	}
	// The QU bit asks devices to answer us directly rather than to the
	// multicast group, which is what lets us listen on an ephemeral port.
	if class := binary.BigEndian.Uint16(q[next+2 : next+4]); class&0x8000 == 0 {
		t.Errorf("question class = %#x, want the QU bit set", class)
	}
}

// TestReadNameCompression covers the pointer form that every real reply uses:
// the second name is stored only as a pointer back to the first.
func TestReadNameCompression(t *testing.T) {
	msg := []byte{
		0x03, 'f', 'o', 'o', 0x05, 'l', 'o', 'c', 'a', 'l', 0x00, // "foo.local." at 0
		0x03, 'b', 'a', 'r', 0xC0, 0x04, // "bar." + pointer to "local." at 4
	}
	name, next, err := readName(msg, 0)
	if err != nil || name != "foo.local." {
		t.Fatalf("readName(0) = %q, %v", name, err)
	}
	if next != 11 {
		t.Errorf("offset after uncompressed name = %d, want 11", next)
	}

	name, next, err = readName(msg, 11)
	if err != nil || name != "bar.local." {
		t.Fatalf("readName(11) = %q, %v", name, err)
	}
	// The offset must be just past the pointer, not past the name it targets.
	if next != len(msg) {
		t.Errorf("offset after compressed name = %d, want %d", next, len(msg))
	}
}

func TestReadNameRejectsPointerLoop(t *testing.T) {
	// A name whose pointer targets itself must terminate, not hang.
	if _, _, err := readName([]byte{0xC0, 0x00}, 0); err == nil {
		t.Error("a self-referential compression pointer should be an error")
	}
}

func TestParseRejectsTruncatedMessages(t *testing.T) {
	full := capturedPackets(t)[0]
	for _, size := range []int{0, 4, 11, len(full) / 2, len(full) - 1} {
		// Truncation must be reported, and above all must not panic on a
		// slice bound: these bytes arrive from anything on the network.
		if err := newRecords().parse(full[:size]); err == nil && size < len(full) {
			t.Errorf("parse of %d truncated bytes returned no error", size)
		}
	}
}

func TestParseIgnoresOtherServices(t *testing.T) {
	// A PTR for a service we did not ask about must not become a device.
	r := newRecords()
	msg := []byte{0, 0, 0x84, 0, 0, 0, 0, 1, 0, 0, 0, 0}
	msg = append(msg, encodeName("_printer._tcp.local.")...)
	msg = append(msg, 0, typePTR, 0, 1, 0, 0, 0, 60)
	target := encodeName("Some Printer._printer._tcp.local.")
	msg = append(msg, byte(len(target)>>8), byte(len(target)))
	msg = append(msg, target...)
	if err := r.parse(msg); err != nil {
		t.Fatal(err)
	}
	if got := len(r.devices()); got != 0 {
		t.Errorf("got %d devices from a non-Cast PTR, want 0", got)
	}
}

func encodeName(name string) []byte {
	var out []byte
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	return append(out, 0)
}
