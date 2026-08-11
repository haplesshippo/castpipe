// Command gen writes mdns-replies.bin, the fixture the mDNS parser is tested
// against. Regenerate with:
//
//	go run testdata/gen.go
//
// The toolchain ignores everything under testdata/, so this file is not part
// of the build and is not vetted with the rest of the package.
//
// The bytes are synthetic but structured exactly like the replies a Chromecast
// sends, because that structure is what the parser has to survive:
//
//   - Names are compressed. The SRV and TXT records refer to the service
//     instance by a pointer back into the PTR record's data, and the A record
//     points at the SRV record's target. A parser that cannot follow a pointer
//     reads nothing at all.
//   - One record type per section: the PTR arrives as an answer, and the
//     SRV/TXT/A that complete it arrive as additionals.
//   - Every device answers separately, and answers more than once, so the
//     parser has to join records across packets and collapse duplicates.
//   - Two cast groups sit on different dynamic ports while the single devices
//     use 8009, which is the case that catches a hardcoded port.
//
// Addresses are from 192.0.2.0/24, which RFC 5737 reserves for documentation.
package main

import (
	"encoding/binary"
	"log"
	"os"
	"strings"
)

const service = "_googlecast._tcp.local."

type device struct {
	instance string
	target   string
	ip       string
	port     uint16
	friendly string
	model    string
}

var devices = []device{
	{"Google-Cast-Group-0a1b2c3d." + service, "0a1b2c3d-0000-4000-8000-000000000001.local.",
		"192.0.2.11", 32001, "Example Speaker Group", "Google Cast Group"},
	{"Google-Cast-Group-2b3c4d5e." + service, "2b3c4d5e-0000-4000-8000-000000000002.local.",
		"192.0.2.12", 32002, "Example Group Two", "Google Cast Group"},
	{"Chromecast-Ultra-4e5f6a7b." + service, "4e5f6a7b-0000-4000-8000-000000000003.local.",
		"192.0.2.12", 8009, "Example Media Player", "Chromecast Ultra"},
	{"Google-Home-8c9d0e1f." + service, "8c9d0e1f-0000-4000-8000-000000000004.local.",
		"192.0.2.11", 8009, "Example Speaker", "Google Home"},
}

func main() {
	var out []byte
	// Each device answers twice, as they do on a real network when the query
	// goes out more than once.
	for range 2 {
		for _, d := range devices {
			out = appendPacket(out, reply(d))
		}
	}
	// A service we never asked about, which must not turn into a device.
	out = appendPacket(out, foreignReply())

	if err := os.WriteFile("testdata/mdns-replies.bin", out, 0o644); err != nil {
		log.Fatal(err)
	}
}

// appendPacket frames one packet as a 4-byte big-endian length and its bytes,
// which is how the fixture stores a sequence of datagrams in one file.
func appendPacket(out, packet []byte) []byte {
	return append(binary.BigEndian.AppendUint32(out, uint32(len(packet))), packet...)
}

func reply(d device) []byte {
	b := &builder{seen: map[string]int{}}
	b.header(1, 3)
	b.record(service, typePTR, func() { b.name(d.instance) })
	b.record(d.instance, typeSRV, func() {
		b.buf = binary.BigEndian.AppendUint16(b.buf, 0) // priority
		b.buf = binary.BigEndian.AppendUint16(b.buf, 0) // weight
		b.buf = binary.BigEndian.AppendUint16(b.buf, d.port)
		b.name(d.target)
	})
	b.record(d.instance, typeTXT, func() {
		// The key/value set a real device sends, trimmed but in the same
		// shape: the parser has to find fn and md among keys it ignores.
		for _, pair := range []string{
			"id=" + strings.ReplaceAll(strings.TrimSuffix(d.target, ".local."), "-", ""),
			"cd=0123456789ABCDEF", "rm=", "ve=05", "ic=/setup/icon.png",
			"fn=" + d.friendly, "md=" + d.model, "ca=4101", "st=0", "bs=FA8FCA000000",
			"nf=1", "rs=",
		} {
			b.buf = append(b.buf, byte(len(pair)))
			b.buf = append(b.buf, pair...)
		}
	})
	b.record(d.target, typeA, func() {
		for _, part := range strings.Split(d.ip, ".") {
			b.buf = append(b.buf, byte(atoi(part)))
		}
	})
	return b.buf
}

func foreignReply() []byte {
	const printer = "_printer._tcp.local."
	b := &builder{seen: map[string]int{}}
	b.header(1, 0)
	b.record(printer, typePTR, func() { b.name("Some Printer." + printer) })
	return b.buf
}

const (
	typeA   = 1
	typePTR = 12
	typeTXT = 16
	typeSRV = 33
)

type builder struct {
	buf  []byte
	seen map[string]int // name -> offset it was first written at
}

func (b *builder) header(answers, additionals uint16) {
	b.buf = binary.BigEndian.AppendUint16(b.buf, 0)      // id
	b.buf = binary.BigEndian.AppendUint16(b.buf, 0x8400) // response, authoritative
	b.buf = binary.BigEndian.AppendUint16(b.buf, 0)      // questions
	b.buf = binary.BigEndian.AppendUint16(b.buf, answers)
	b.buf = binary.BigEndian.AppendUint16(b.buf, 0) // authority
	b.buf = binary.BigEndian.AppendUint16(b.buf, additionals)
}

func (b *builder) record(name string, recordType uint16, rdata func()) {
	b.name(name)
	b.buf = binary.BigEndian.AppendUint16(b.buf, recordType)
	b.buf = binary.BigEndian.AppendUint16(b.buf, 1)   // class IN
	b.buf = binary.BigEndian.AppendUint32(b.buf, 120) // ttl
	sizeAt := len(b.buf)
	b.buf = append(b.buf, 0, 0)
	start := len(b.buf)
	rdata()
	binary.BigEndian.PutUint16(b.buf[sizeAt:], uint16(len(b.buf)-start))
}

// name writes a domain name, pointing back at any suffix already in the
// packet rather than repeating it - the compression every real responder uses.
func (b *builder) name(n string) {
	for n != "" {
		if off, ok := b.seen[n]; ok {
			b.buf = append(b.buf, byte(0xC0|off>>8), byte(off))
			return
		}
		b.seen[n] = len(b.buf)
		label, rest, _ := strings.Cut(n, ".")
		b.buf = append(b.buf, byte(len(label)))
		b.buf = append(b.buf, label...)
		n = rest
	}
	b.buf = append(b.buf, 0)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
