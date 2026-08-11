// Command castpipe casts one local media file to a Chromecast on the local
// network. The device fetches the media from us over HTTP, so `play` serves
// the file for as long as it plays; Ctrl-C stops playback and exits.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	discoverTimeout = 3 * time.Second
	dialTimeout     = 5 * time.Second
	replyTimeout    = 10 * time.Second
	loadTimeout     = 30 * time.Second
)

const usage = `castpipe casts one local media file to a Chromecast.

  castpipe play <file> [--to <device>]   cast a file; Ctrl-C stops playback
  castpipe devices                       list Cast devices on this network
  castpipe stop [--to <device>]          stop whatever the device is playing

--to takes a device or group name, matched case-insensitively, or a literal
host:port for networks where mDNS does not get through. It can be left out
when there is only one device on the network.`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "castpipe: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("no command given\n\n" + usage)
	}
	command, rest, to, err := parseArgs(args)
	if err != nil {
		return err
	}
	switch command {
	case "play":
		if len(rest) != 1 {
			return errors.New("usage: castpipe play <file> [--to <device>]")
		}
		return play(rest[0], to)
	case "devices":
		if len(rest) != 0 || to != "" {
			return errors.New("usage: castpipe devices")
		}
		return listDevices()
	case "stop":
		if len(rest) != 0 {
			return errors.New("usage: castpipe stop [--to <device>]")
		}
		return stop(to)
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q\n\n%s", command, usage)
}

// parseArgs pulls --to out from wherever it appears, so that both
// `play --to x file` and `play file --to x` work.
func parseArgs(args []string) (command string, rest []string, to string, err error) {
	command = args[0]
	for i := 1; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--to":
			if i++; i == len(args) {
				return "", nil, "", errors.New("--to needs a device name")
			}
			to = args[i]
		case strings.HasPrefix(arg, "--to="):
			to = strings.TrimPrefix(arg, "--to=")
		default:
			rest = append(rest, arg)
		}
	}
	return command, rest, to, nil
}

func listDevices() error {
	devices, err := Discover(discoverTimeout)
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		return noDevices()
	}
	for _, d := range devices {
		fmt.Printf("%-24s %-21s %s\n", d.Name, d.Addr(), d.Model)
	}
	return nil
}

func play(path, to string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// The format check comes first: it is certain, costs nothing, and a file
	// the receiver can never play is worth saying so about before we spend a
	// discovery timeout finding a device to disappoint.
	mediaType, err := contentType(path)
	if err != nil {
		return err
	}
	if info, err := os.Stat(path); err != nil {
		return err
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a file", path)
	}
	device, err := resolve(to)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	server, err := serveFile(path, mediaType, device.Host)
	if err != nil {
		return err
	}
	defer server.Close()

	session, err := connect(device)
	if err != nil {
		return err
	}
	defer session.Close()
	if ctx.Err() != nil {
		return nil
	}

	transport, sessionID, err := session.launch(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = session.quit(sessionID)
			_ = session.Close()
		case <-done:
		}
	}()
	if ctx.Err() != nil {
		return nil
	}
	title := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	duration, err := session.load(transport, server.url, mediaType, title)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		// Naming the URL matters: when a device refuses, the usual cause is
		// that it could not reach this address, and that is invisible here.
		return fmt.Errorf("%w\n  it was asked to fetch %s", err, server.url)
	}
	if !server.Fetched() {
		if ctx.Err() != nil {
			return nil
		}
		return errors.New("the device reports playing but never fetched the file from us")
	}
	where := device.Name
	if where != device.Addr() {
		where += " (" + device.Addr() + ")"
	}
	fmt.Printf("playing %s%s\n     on %s\n", title, bracket(duration), where)

	// Playback lives inside this process. Stopping it on the way out is the
	// honest thing to do: the moment we exit the file stops being served and
	// the device would stall part-way through anyway.
	err = session.follow(sessionID)
	switch {
	case ctx.Err() != nil:
		fmt.Println("stopped")
	case errors.Is(err, errTakenOver):
		fmt.Println("stopped on the device")
	case err != nil:
		return err
	default:
		fmt.Println("finished")
	}
	return nil
}

func stop(to string) error {
	device, err := resolve(to)
	if err != nil {
		return err
	}
	session, err := connect(device)
	if err != nil {
		return err
	}
	defer session.Close()

	sessionID, err := session.running()
	if err != nil {
		return err
	}
	if sessionID == "" {
		fmt.Printf("%s is already idle\n", device.Name)
		return nil
	}
	if err := session.quit(sessionID); err != nil {
		return err
	}
	fmt.Printf("stopped %s\n", device.Name)
	return nil
}

// resolve turns --to into one device. With no --to and one device on the
// network the choice is unambiguous; with several it refuses to guess.
func resolve(to string) (Device, error) {
	if host, port, err := net.SplitHostPort(to); err == nil && host != "" {
		number, err := strconv.Atoi(port)
		if err != nil {
			return Device{}, fmt.Errorf("%q is not a host:port", to)
		}
		return Device{Name: to, Host: host, Port: number}, nil
	}
	devices, err := Discover(discoverTimeout)
	if err != nil {
		return Device{}, err
	}
	if len(devices) == 0 {
		return Device{}, noDevices()
	}
	if to == "" {
		if len(devices) == 1 {
			return devices[0], nil
		}
		return Device{}, fmt.Errorf("%d devices on this network; choose one with --to:\n%s",
			len(devices), names(devices))
	}
	var matches []Device
	for _, d := range devices {
		if strings.EqualFold(d.Name, to) {
			return d, nil
		}
		if strings.Contains(strings.ToLower(d.Name), strings.ToLower(to)) {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return Device{}, fmt.Errorf("no device called %q; this network has:\n%s", to, names(devices))
	}
	return Device{}, fmt.Errorf("%q matches %d devices:\n%s", to, len(matches), names(matches))
}

func names(devices []Device) string {
	var b strings.Builder
	for _, d := range devices {
		fmt.Fprintf(&b, "  %s\n", d.Name)
	}
	return strings.TrimRight(b.String(), "\n")
}

// noDevices is the message people see most often when something is wrong, so
// it names the likely cause rather than stopping at the symptom.
func noDevices() error {
	err := errors.New("no Cast devices answered on this network")
	if runtime.GOOS != "darwin" {
		return err
	}
	return fmt.Errorf("%w\n\n"+
		"If there is definitely one there, macOS is probably blocking castpipe's\n"+
		"access to the local network. Allow it under System Settings > Privacy &\n"+
		"Security > Local Network, then try again.", err)
}

func bracket(duration float64) string {
	if duration <= 0 {
		return ""
	}
	return " (" + clock(duration) + ")"
}

func clock(seconds float64) string {
	whole := int(seconds)
	return fmt.Sprintf("%d:%02d", whole/60, whole%60)
}
