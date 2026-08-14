# Project agent memory

This file is the project's committed home for project-intrinsic agent knowledge: build, test, release, architecture, and sharp-edge notes that should travel with the code.

## What this project is

A deliberately small CLI that casts one local media file to a Chromecast.
Four non-test Go files, standard library only. See `README.md` for the command
surface and `README.md#how-it-works` for what each file owns.

**Smallness is a requirement, not a style preference.** `go.mod` must keep an
empty require list: the mDNS query, the castv2 protobuf framing and the file
server are all hand-rolled on purpose. Do not add a dependency, and do not grow
the command surface beyond `play`, `devices` and `stop`.

## Build and test

```sh
go build -o castpipe .   # build
go test ./...            # unit tests, no network needed
go vet ./... && gofmt -l .
```

CI is one job in `.github/workflows/ci.yml` running gofmt, vet and test.

## Sharp edges, all of them learned the hard way against real hardware

**macOS Local Network Privacy will block you, and it lies.** An unsigned or
non-Apple binary cannot open connections to LAN addresses or send mDNS
multicast; both fail with `EHOSTUNREACH` / "no route to host", which reads as a
routing fault. `go run`/`go build` binaries are affected, so **castpipe cannot
reach a Cast device from a dev machine until Local Network access is granted to
it in System Settings**. Apple-signed binaries (`nc`, `curl`, `/usr/bin/python3`)
are exempt, which is the fastest way to prove the device is actually reachable.
`localNetworkHint` in `serve.go` translates the error for users.

To test against hardware without that grant, relay through Apple-signed Python:
listen on the machine's **LAN** address (not loopback - castpipe derives the
media URL from the route to the address it was given, and the device must be
able to fetch that URL back) and forward to the device, then use
`--to <lan-ip>:<relay-port>`.

**A cast group does not answer on 8009.** It answers on a dynamic port on the
group leader, which changes. Always take host and port from the SRV record;
never cache or default the port. `testdata/mdns-replies.bin` covers both cases
and is what `TestParseReplies` asserts against; regenerate it with
`go run testdata/gen.go`. The fixture is synthetic on purpose - this repo is
public, so it must never carry a real household's device names or addresses -
but it is structured like the real thing, and it is verified to fail if the
parser stops following compression pointers, defaults the port, or ignores the
additionals section.

**The URL handed to the device is escaped; the path the handler receives is
not.** `net/http` decodes `%20` back to a space before the handler sees it, so
these must be compared in the same spelling. Getting this wrong 404s every file
whose name contains a space and shows up only as `LOAD_FAILED` from the device.
See the two path spellings in `serveFile`.

**Byte ranges are mandatory.** The device's first request carries
`Range: bytes=0-` and it seeks by byte offset; `http.ServeFile` covers this.

**The device resets connections rather than closing them**, and it stops being
yours without warning when another sender takes over. `follow` watches the
receiver namespace for its own session disappearing - without that, `play`
blocks forever serving a file nobody is reading.

## Verifying a change actually works

Unit tests do not prove a cast works. Cast to a real device, and confirm
playback from something other than castpipe's own output: connect separately
and check `MEDIA_STATUS` shows `PLAYING` with `currentTime` advancing, and that
`contentId` is castpipe's own URL. A zero exit code is not evidence.

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
