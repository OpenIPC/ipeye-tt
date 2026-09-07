# CLAUDE.md

Guidance for AI assistants working in ipeye-tt.

## What this is

A bench stand-in for an RTSP cloud a camera dials *out* to. It plays the
balancer (an HTTPS node lookup) and the RTSP client (stock `deepch/vdk` over
the connection the camera opens), measures the pushed stream, and prints a
per-run verdict with a non-zero exit on failure. Siblings: `OpenIPC/onvif-tt`,
`OpenIPC/sip-tt`.

## Build and run

- `./run.sh build` / `./run.sh test ./...` — build and test inside
  `golang:1.24`; there is no host Go dependency.
- `./run.sh -mode reverse -advertise <ip> ...` — the camera dials in.
- `./run.sh -mode forward -url rtsp://... ...` — dial the camera; a control.
- `scripts/cam-{setup,run,restore}.sh` — point a camera at the harness over
  SSH and put it back. `cam-run.sh` runs a binary from `/tmp` and never
  replaces the installed one; use `scp -O` for any hand copy (no SFTP on these
  cameras).

## Layout

`cmd/ipeye-tt` wiring and verdict · `internal/balancer` node lookup ·
`internal/bridge` accept-and-splice plus conditions · `internal/probe` the vdk
client · `internal/wire` interleaved/response/SDP parsing and per-track stats ·
`internal/report` thresholds and output · `internal/certs` the balancer CA.

## Rules

- vdk is a dependency, unmodified. Do not vendor or patch it.
- A `FAIL` must be backed by a number in the JSON.
- Default runs measure a good link; `-throttle`/`-stall` model a bad one and
  relax the reconnect/gap checks accordingly.
- No real cloud credentials, keys, or endpoints belong in this tree.
- Branch and PR; stage named paths.
