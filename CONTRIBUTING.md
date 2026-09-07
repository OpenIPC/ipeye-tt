# Contributing to ipeye-tt

ipeye-tt is a measuring tool. A change is worth making if it lets the tool see
something true about a camera's pushed stream that it could not see before, or
report what it sees more clearly.

## Build and test

Everything runs in the official Go image; no host Go toolchain is needed.

```sh
./run.sh build      # build bin/ipeye-tt
./run.sh test ./... # run the unit tests
```

The unit tests cover the wire parser (frame/response splitting, the keepalive
acknowledgement, desync recovery, SDP rate parsing) and run without a camera.
Anything that can be tested without hardware should be.

## House rules

- **Model the cloud, do not fake the camera.** The RTSP client is stock
  `deepch/vdk`, used as a dependency and never vendored or patched, so that
  what the tool accepts is what a real vdk-based cloud accepts. If vdk needs to
  behave differently, the fix is a flag on our side or an upstream change, not
  a private copy.
- **Every finding is a measurement.** A `FAIL` names a number and where it came
  from. Do not report a verdict the JSON does not substantiate.
- **Impairments are opt-in.** A default run measures a camera on a good link.
  `-throttle`, `-stall` and the like model a bad one, and under them the
  verdict stops counting the impairment's own consequences as faults.
- **No real credentials or endpoints.** The balancer answers with its own
  address; RTSP credentials come from the command line. Nothing here carries a
  key or account for any real cloud.

## Commits

Work on a branch and open a pull request, even for small changes. Stage named
paths, not `git add -A`. Keep the subject in the imperative and say why in the
body.
