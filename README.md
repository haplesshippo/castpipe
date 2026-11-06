# castpipe

Cast one local media file to a Chromecast from the command line.

One Go binary, standard library only: no dependencies, no daemon, no
configuration file. The whole thing is about a thousand lines across four
files, and it is meant to be read.

```
$ castpipe play ~/Music/16\ here\ comes\ the\ sun.mp3 --to "Living Room Speakers"
playing 16 here comes the sun (3:05)
     on Living Room Speakers (192.168.1.113:32064)
```

## Install

```sh
go install github.com/haplesshippo/castpipe@latest
```

Or build it yourself:

```sh
git clone https://github.com/haplesshippo/castpipe
cd castpipe && go build -o castpipe .
```

Go 1.24 or newer. There is nothing else to install.

## Commands

There are four, and that is the whole surface.

### `castpipe devices`

Lists what answered on the network. Name, address and model:

```
$ castpipe devices
Kitchen Speaker          192.168.1.247:8009    Google Home
Living Room Speakers     192.168.1.113:32064   Google Cast Group
TV                       192.168.1.217:8009    Chromecast Ultra
```

Note the ports. A speaker answers on 8009, but **a cast group answers on a
dynamic port** - 32064 here - chosen by whichever device is leading the group,
and it changes. castpipe always reads the port from the mDNS record. Tools that
assume 8009 appear to work and quietly cast to one speaker instead of the group.

### `castpipe play <file>`

Casts the file and stays running until it ends.

```
$ castpipe play ~/Music/track.mp3 --to TV
playing track (3:05)
     on TV (192.168.1.217:8009)
finished
```

`--to` matches a device or group name, case-insensitively, in full or as a
substring, so `--to "living room"` is enough. It also accepts a literal
`host:port` for networks where mDNS does not get through. If there is exactly
one device on the network you can leave `--to` out; if there are several,
castpipe lists them and exits rather than guessing which room to make noise in.

**The process has to stay alive.** A Chromecast does not read your disk: it
fetches the file over HTTP from the machine that told it to play. castpipe
serves it on an address the device can route to, and that server lives inside
the `play` command.

**Ctrl-C stops playback on the device and exits.** That is deliberate. The
alternative - leaving the device playing - cannot work, because the file stops
being served the moment castpipe exits, so the track would stall part-way
through. Playback also ends if something else takes the device over:

```
$ castpipe play ~/Music/track.mp3 --to TV
playing track (3:05)
     on TV (192.168.1.217:8009)
stopped on the device
```

### `castpipe stop`

Stops whatever is playing and returns the device to idle.

```
$ castpipe stop --to "Living Room Speakers"
stopped Living Room Speakers

$ castpipe stop --to "Living Room Speakers"
Living Room Speakers is already idle
```

## What it plays

castpipe checks the file extension against what Google's Default Media Receiver
can actually decode, and refuses anything else up front instead of handing the
device a URL and hoping:

```
$ castpipe play ~/Video/film.mkv --to TV
castpipe: a Chromecast cannot play .mkv files; supported: .aac .flac .m4a .m4v
.mp3 .mp4 .oga .ogg .opus .wav .webm
```

`.mkv` is the common surprise. It is not on the receiver's container list, and
neither is DTS or AC-3 audio on a device that cannot pass it through. If the
device rejects a file anyway, castpipe reports that and exits non-zero rather
than sitting there looking successful.

## Troubleshooting

### macOS says nothing is on the network, or "no route to host"

**This is the one that will get you, and macOS lies about it.**

On macOS, an application that has not been granted Local Network access cannot
reach LAN addresses at all - and the failure is reported as `EHOSTUNREACH`,
"no route to host". That is a routing error, so the natural response is to go
and debug your router, your Wi-Fi, or your firewall. None of those are the
problem. The device is reachable; your binary is not allowed to reach it.

It affects the mDNS query too, so the usual symptom is `castpipe devices`
finding nothing at all while the speaker plays happily from your phone.

**Fix:** System Settings → Privacy & Security → Local Network, and allow
castpipe (or the terminal you are running it from). Then run it again.

castpipe detects this case and says so instead of repeating the OS's
misleading error. Apple's own binaries - `nc`, `curl`, `/usr/bin/python3` - are
exempt from the restriction, which is why `nc -vz <device> 8009` can succeed at
the same moment castpipe cannot connect. That is a difference in code signature,
not in your network.

### The device says it cannot play the file

Check the format list above. Beyond that, castpipe prints the URL it asked the
device to fetch; if that address is not one the device can route to - a VPN
interface, say - that is the thing to look at.

## Scope

castpipe casts one file to one device. Deliberately absent:

- **No transcoding.** If the receiver cannot decode it, castpipe says so.
- **No queue, no playlist, no shuffle.** One file per invocation.
- **No seek, pause or volume control.** Play and stop.
- **No subtitles.**
- **No daemon and no config file.** Nothing runs when you are not casting.
- **No YouTube, Netflix or any other app.** Local files only, through the
  Default Media Receiver.
- **No video device features** beyond playing a supported video file.

If you want those, [catt](https://github.com/skorokithakis/catt) and
[go-chromecast](https://github.com/vishen/go-chromecast) are larger tools that
cover more ground.

## How it works

Four files, and each one is the whole of its subject:

| File | What it does |
|---|---|
| `mdns.go` | Sends one multicast DNS question and joins the PTR/SRV/TXT/A records in the replies into devices. |
| `cast.go` | The castv2 protocol: length-prefixed protobuf frames over TLS, carrying JSON. The protobuf encoding is hand-written; there is only one message type. |
| `serve.go` | The HTTP server the device fetches from, plus picking a local address it can route to. |
| `main.go` | The three commands. |

Two details worth knowing if you are reading the code. The device sends
`Range: bytes=0-` on its very first request and seeks by asking for a byte
offset, so byte-range support is not optional - `http.ServeFile` handles it.
And the device resets connections rather than closing them politely, so an
abrupt end to a connection is normal rather than an error.

## Tests

```sh
go test ./...
```

The mDNS parser is tested against `testdata/mdns-replies.bin`, whole replies
laid out the way a Chromecast lays them out: names compressed with pointers,
the PTR arriving as an answer while the SRV/TXT/A that complete it arrive as
additionals, every device answering more than once, and two cast groups on
different dynamic ports alongside devices on 8009. `testdata/gen.go` builds it
and explains what each part is there to catch.

The protobuf encoding is pinned byte for byte, because a wrong field number
still round-trips through your own decoder while every device silently ignores
you.

## Contributing

Keep code files small: target 500 physical lines or fewer. Above 800 lines,
the file-length check warns but passes; above 1000, it fails. Exactly 800 is
quiet and exactly 1000 warns. Blank lines and comments count. LF and CRLF
each end one line; a final unterminated line counts once.

Run the same checks as CI before submitting changes:

```sh
test -z "$(gofmt -l .)"
go vet ./...
go test -v ./...
```

The file-length check runs in `go test ./...`; use `-v` to see passing
warnings, as CI does. It scans all authored `.go` files, including tests,
new local files and `testdata/gen.go`, plus `.github/workflows/*.yml` and
`*.yaml` command configuration and `.no-mistakes.yaml` if present. It skips
Go files with the standard `// Code generated ... DO NOT EDIT.` header,
Git metadata and vendor code. Prose, fixture data, lockfiles and binaries
are outside the code policy; `testdata`
itself is not exempt. Compiled build output such as `castpipe` is excluded,
but authored Go tools under directories named `build` or `dist` still count.
The check uses only the standard library.

## License

MIT. See [LICENSE](LICENSE).
