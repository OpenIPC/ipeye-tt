# Bench setup

## What you need

- A camera running the OpenIPC firmware with the reverse-RTSP cloud connector,
  reachable over SSH, on the same network as the host running `ipeye-tt`.
- Docker on the host (the harness runs inside `golang:1.24`; `run.sh` uses the
  host network so the balancer can bind port 443 and the camera can reach the
  listener).
- The IP the camera can reach the host at (`-advertise`).

## The exchange, end to end

1. The camera resolves the cloud's balancer name and asks it, over HTTPS, for a
   node: `GET /balancer/server/<id>?vendor=...&model=...`. `ipeye-tt`'s
   balancer answers `{"code":"200","message":"<advertise-ip>|<rtsp-port>"}`.
2. The camera dials that TCP port and writes `REGISTER={...}` (JSON, no
   terminator). `ipeye-tt` reads it and logs it.
3. `ipeye-tt` hands the connection to a stock `deepch/vdk` RTSP client, which
   drives `OPTIONS`/`DESCRIBE`/`SETUP`/`PLAY` and consumes interleaved RTP.
4. Every 25 s vdk sends an `OPTIONS` keepalive; `ipeye-tt` also sends the
   cloud's own `CRLFCRLF` keepalive and expects `ok`.

Because the balancer is asked for by *name* and reached over TLS, the camera
must (a) resolve that name to the harness and (b) trust the harness's
certificate. `scripts/cam-setup.sh` does both: it appends the generated CA to
the camera's trust bundle and adds a hosts entry. The certificate is issued for
the name, not an address — a TLS client does not match an IP SAN.

## Point a camera at the harness

```sh
# Mint the CA (first reverse run does this too):
./run.sh -mode reverse -advertise <host-ip> -duration 1s

# Redirect the camera and enable the connector (backs up what it changes):
scripts/cam-setup.sh <camera-host> <host-ip>

# Run the harness; start or swap the camera's binary from /tmp meanwhile:
./run.sh -mode reverse -advertise <host-ip> -duration 10m -json out/run.json &
scripts/cam-run.sh <camera-host> ./path/to/majestic

# Put everything back:
scripts/cam-restore.sh <camera-host>
```

`cam-run.sh` never touches the installed binary: it copies to
`/tmp/majestic-ipeye`, stops the stock service, runs from there, and
`cam-restore.sh` (or `cam-run.sh <host> stock`) starts the stock service
again. Use `scp -O` for any hand copy — these cameras have no SFTP server.

## Modelling a bad uplink

A LAN bench never shows what a congested WAN path does. Force it:

- `-throttle <kbit/s>` reads from the camera no faster than that, so the
  camera's send buffer fills the way it would behind a slow uplink.
- `-stall <dur>` stops reading entirely for a while.
- `-keepalive-glued` sends the keepalive in the same write as the next
  request, to test a server that only recognises it as a standalone packet.

Under `-throttle`/`-stall` the verdict does not count reconnects or gaps as
failures by themselves — they are the expected consequence of the impairment —
but it still checks RTP integrity, G.711 rate honesty, and framing.
