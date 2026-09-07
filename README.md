# ipeye-tt

Open-source, headless, CI- and AI-friendly **reverse-RTSP cloud test tool**.

`ipeye-tt` stands in for an RTSP cloud that a camera dials *out* to, so the
stream a camera pushes can be measured on a bench instead of guessed at from a
web player. It plays the cloud's two parts — the balancer a camera asks for a
node address, and the RTSP client that then pulls video and audio back over
the connection the camera opened — records everything that crosses the wire,
and prints a per-run verdict. It emits JSON and a non-zero exit code, needs no
GUI, and is meant to be run by CI and read by agents as readily as by people.

It is a sibling of [onvif-tt](https://github.com/OpenIPC/onvif-tt) and
[sip-tt](https://github.com/OpenIPC/sip-tt), and exists for the same reason:
delivery defects are behavioural, they hide from unit tests, and finding them
by watching a cloud player does not scale.

## Why

A camera that looks perfect in its own web preview can still deliver a broken
stream to a cloud, because the two travel different paths: the preview is
local and pulled, the cloud stream is pushed over a single long-lived TCP
connection across the open internet. "Wild buffering", audio that cuts out,
and a stream that reconnects more than it runs are all things a cloud sees and
a local viewer never does. This tool puts a measuring cloud on the bench.

The push protocol it models is the one an OpenIPC camera speaks: the camera
asks a balancer over HTTPS for a node, dials that node over TCP, announces
itself with a one-line `REGISTER={...}`, and from then on **the cloud drives
RTSP over that same socket** — `DESCRIBE`, `SETUP` with
`RTP/AVP/TCP;interleaved`, `PLAY`, and interleaved RTP/RTCP back to the cloud,
kept alive by a tiny `CRLFCRLF` / `ok` exchange. The reference cloud is built
on the Go library [`deepch/vdk`](https://github.com/deepch/vdk), and this tool
drives that same library unmodified, so what it accepts and rejects is what a
real vdk-based cloud accepts and rejects.

## What it measures

Per session, on the bytes the camera actually sends:

- **Delivery**: bytes/s per track, the longest silence on each track and when
  it happened, and every reconnect.
- **RTP integrity**: sequence gaps and backward jumps on a TCP session (there
  should be none), timestamp monotonicity and wrap, marker cadence.
- **Audio/video timeline**: what the vdk client computes for each track's
  packet time, the initial A/V offset, and — separately — how fast that offset
  *grows*, which is what a clock-rate mismatch produces.
- **G.711 rate honesty**: for a PCMA/PCMU track, whether the payload bytes per
  second and the RTP timestamp advance match the clock rate the SDP declares.
- **Framing faults**: any byte on the session that is neither an interleaved
  frame nor an RTSP response (a vdk client treats one as a desync and drops the
  connection), and any unanswered keepalive.

It can also apply conditions a bench link otherwise won't: read the camera
slower than it sends (`-throttle`), stop reading for a while (`-stall`), and
send the keepalive glued to the next request (`-keepalive-glued`) — the shapes
a marginal uplink and a coalescing TCP stack produce.

## Run it

Everything runs in the official Go image; `run.sh` wraps Docker and uses the
host network so the balancer can own port 443 and the camera can reach the
listener.

```sh
./run.sh build

# Reverse (the real thing): the camera dials in. -advertise is the IP the
# camera can reach this host at.
./run.sh -mode reverse -advertise 10.0.0.2 -duration 10m -json out/run.json

# Forward (a control): dial the camera's own RTSP port and run the same probe.
./run.sh -mode forward -url rtsp://root:pass@camera:554/stream=0 -duration 2m
```

`-mode reverse` mints a certificate authority under `certs/` on first run.
Point the camera at the harness and add that CA to its trust store — the
`scripts/cam-*.sh` helpers do this over SSH and put everything back
afterwards:

```sh
scripts/cam-setup.sh   camera-host 10.0.0.2        # hosts + CA + connector on
scripts/cam-run.sh     camera-host ./majestic       # run a binary from /tmp
scripts/cam-restore.sh camera-host                  # undo everything
```

The verdict is `PASS`/`FAIL` on stdout and, with `-json`, the full record. The
process exits non-zero on `FAIL`, so it drops straight into CI.

## Layout

| Path | What |
| --- | --- |
| `cmd/ipeye-tt` | flags, wiring, the verdict |
| `internal/balancer` | the HTTPS node-lookup endpoint |
| `internal/bridge` | accept the camera, splice it to vdk, apply conditions |
| `internal/probe` | drive the stock vdk client, record its timelines |
| `internal/wire` | parse interleaved frames, responses, SDP; per-track stats |
| `internal/report` | thresholds, findings, JSON and text output |
| `internal/certs` | the balancer's CA and server certificate |
| `scripts/` | point a camera at the harness and back |

## Scope

This tool exercises and measures a camera. It is not a cloud service and
carries no account, key, or credential for any real one; the balancer answers
every lookup with its own address, and the RTSP credentials it presents are
whatever you pass on the command line. It speaks the reverse-RTSP protocol as
observed on the wire.

## Licence

MIT. `deepch/vdk` is MIT-licensed and used unmodified as a dependency.
