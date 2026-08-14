package main

// Multicast DNS, only as much of it as finding a Chromecast needs.
//
// One question - PTR for _googlecast._tcp.local. - goes out on every
// multicast-capable interface, and the PTR/SRV/TXT/A records in the replies are
// joined into devices. The question sets the QU bit so devices answer us
// directly, which saves binding port 5353; on macOS mDNSResponder holds it.

import (
	"encoding/binary"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Device is one Cast receiver, which may be a single speaker or a group.
type Device struct {
	Name  string // friendly name, e.g. "Living Room Speakers"
	Model string // e.g. "Google Cast Group", "Chromecast Ultra"
	Host  string // IPv4 address
	Port  int    // dynamic, and not 8009 for a group: always take it from SRV
}

// Addr is the host:port to open a castv2 connection to.
func (d Device) Addr() string { return net.JoinHostPort(d.Host, strconv.Itoa(d.Port)) }

const castService = "_googlecast._tcp.local."
const (
	typeA   = 1
	typePTR = 12
	typeTXT = 16
	typeSRV = 33
)

var errMalformed = errors.New("malformed DNS message")

// Discover asks every usable interface and joins the answers that arrive
// before the timeout. It never returns a partially-resolved device.
func Discover(timeout time.Duration) ([]Device, error) {
	sources := multicastSources()
	if len(sources) == 0 {
		return nil, errors.New("no multicast-capable network interface is up")
	}
	r := newRecords()
	var wg sync.WaitGroup
	errs := make([]error, len(sources))
	for i, src := range sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = browse(src, timeout, r)
		}()
	}
	wg.Wait()

	devices := r.devices()
	if len(devices) == 0 {
		// A send failure is only worth reporting when it explains the silence:
		// on a machine with a VPN and several interfaces, most of them
		// legitimately answer nothing.
		for _, err := range errs {
			if err != nil {
				return nil, localNetworkHint(err)
			}
		}
	}
	return devices, nil
}

// browse asks from one interface address and collects replies until the
// timeout, asking a second time on the way because mDNS is unacknowledged.
func browse(src net.IP, timeout time.Duration, r *records) error {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: src})
	if err != nil {
		return err
	}
	defer conn.Close()

	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	if _, err := conn.WriteToUDP(query(), group); err != nil {
		return err
	}
	again := time.AfterFunc(timeout/2, func() { _, _ = conn.WriteToUDP(query(), group) })
	defer again.Stop()

	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	buf := make([]byte, 9000)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return nil // the deadline, i.e. we are done listening
		}
		_ = r.parse(buf[:n])
	}
}

// query builds a PTR question for the Cast service, with the QU (reply to me
// directly) bit set in the class field.
func query() []byte {
	msg := []byte{0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, label := range strings.Split(strings.TrimSuffix(castService, "."), ".") {
		msg = append(msg, byte(len(label)))
		msg = append(msg, label...)
	}
	return append(msg, 0, 0, typePTR, 0x80, 0x01)
}

func multicastSources() []net.IP {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, iface := range interfaces {
		const want = net.FlagUp | net.FlagMulticast
		if iface.Flags&want != want || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			n, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if ip := n.IP.To4(); ip != nil && !ip.IsLinkLocalUnicast() {
				out = append(out, ip)
			}
		}
	}
	return out
}

type hostPort struct {
	host string
	port int
}

// records accumulates the four record types across every reply on every
// interface. Duplicates collapse because everything is keyed by name.
type records struct {
	mu   sync.Mutex
	ptr  map[string]bool              // service instance names
	srv  map[string]hostPort          // instance -> target host and port
	txt  map[string]map[string]string // instance -> TXT key/value pairs
	addr map[string]string            // target host -> IPv4 address
}

func newRecords() *records {
	return &records{
		ptr:  map[string]bool{},
		srv:  map[string]hostPort{},
		txt:  map[string]map[string]string{},
		addr: map[string]string{},
	}
}

// devices joins the records. An instance missing any link of the chain
// (instance -> SRV -> A) is dropped rather than guessed at.
func (r *records) devices() []Device {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Device
	for instance := range r.ptr {
		srv, ok := r.srv[instance]
		if !ok {
			continue
		}
		ip, ok := r.addr[srv.host]
		if !ok {
			continue
		}
		txt := r.txt[instance]
		name := txt["fn"]
		if name == "" {
			name = strings.TrimSuffix(instance, "."+castService)
		}
		out = append(out, Device{Name: name, Model: txt["md"], Host: ip, Port: srv.port})
	}
	slices.SortFunc(out, func(a, b Device) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (r *records) parse(msg []byte) error {
	if len(msg) < 12 {
		return errMalformed
	}
	questions := int(binary.BigEndian.Uint16(msg[4:6]))
	answers := int(binary.BigEndian.Uint16(msg[6:8])) +
		int(binary.BigEndian.Uint16(msg[8:10])) +
		int(binary.BigEndian.Uint16(msg[10:12]))

	off := 12
	for range questions {
		_, next, err := readName(msg, off)
		if err != nil {
			return err
		}
		off = next + 4
	}
	for range answers {
		name, next, err := readName(msg, off)
		if err != nil {
			return err
		}
		if next+10 > len(msg) {
			return errMalformed
		}
		recordType := binary.BigEndian.Uint16(msg[next : next+2])
		length := int(binary.BigEndian.Uint16(msg[next+8 : next+10]))
		off = next + 10
		if off+length > len(msg) {
			return errMalformed
		}
		r.add(msg, name, recordType, off, length)
		off += length
	}
	return nil
}

// add stores one resource record. The record data is passed as an offset into
// the whole message because names inside PTR and SRV records may use
// compression pointers, which only mean anything against the whole message.
func (r *records) add(msg []byte, name string, recordType uint16, off, length int) {
	rdata := msg[off : off+length]
	r.mu.Lock()
	defer r.mu.Unlock()
	switch recordType {
	case typePTR:
		if !strings.EqualFold(name, castService) {
			return
		}
		if instance, _, err := readName(msg, off); err == nil {
			r.ptr[instance] = true
		}
	case typeSRV:
		if length < 6 {
			return
		}
		if target, _, err := readName(msg, off+6); err == nil {
			r.srv[name] = hostPort{target, int(binary.BigEndian.Uint16(rdata[4:6]))}
		}
	case typeTXT:
		pairs := map[string]string{}
		for i := 0; i < length; {
			size := int(rdata[i])
			if i++; i+size > length {
				break
			}
			if key, value, ok := strings.Cut(string(rdata[i:i+size]), "="); ok {
				pairs[key] = value
			}
			i += size
		}
		if len(pairs) > 0 {
			r.txt[name] = pairs
		}
	case typeA:
		if length == 4 {
			r.addr[name] = net.IP(rdata).String()
		}
	}
}

// readName decodes a possibly-compressed domain name. The offset it returns is
// the one just past the name in the current record, which after following a
// compression pointer is not where decoding stopped.
func readName(msg []byte, off int) (string, int, error) {
	var name strings.Builder
	end := -1
	for jumps := 0; ; {
		if off >= len(msg) {
			return "", 0, errMalformed
		}
		length := int(msg[off])
		if length == 0 {
			off++
			break
		}
		if length&0xC0 == 0xC0 {
			if off+1 >= len(msg) {
				return "", 0, errMalformed
			}
			if jumps++; jumps > 16 {
				return "", 0, errMalformed
			}
			if end < 0 {
				end = off + 2
			}
			off = (length&0x3F)<<8 | int(msg[off+1])
			continue
		}
		if off+1+length > len(msg) {
			return "", 0, errMalformed
		}
		name.Write(msg[off+1 : off+1+length])
		name.WriteByte('.')
		off += 1 + length
	}
	if end >= 0 {
		return name.String(), end, nil
	}
	return name.String(), off, nil
}
