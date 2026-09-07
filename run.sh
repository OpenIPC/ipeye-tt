#!/bin/sh
# Build and run ipeye-tt inside the official Go image, on the host network so
# the fake balancer can own port 443 and the camera can reach the listener.
#
#   ./run.sh build
#   ./run.sh -mode reverse -advertise 10.0.0.2 -duration 10m -json out/run.json
#   ./run.sh -mode forward -url rtsp://root:pw@camera:554/stream=0
set -e
cd "$(dirname "$0")"
IMAGE=${IPEYE_TT_IMAGE:-golang:1.24}
MODCACHE=${IPEYE_TT_MODCACHE:-ipeye-tt-gomod}
mkdir -p bin out certs

build() {
  docker run --rm -v "$PWD":/src -w /src -v "$MODCACHE":/go/pkg/mod \
    -e GOFLAGS=-mod=mod -e CGO_ENABLED=0 "$IMAGE" \
    sh -c 'go mod tidy >/dev/null && go build -trimpath -o bin/ipeye-tt ./cmd/ipeye-tt'
}

case "$1" in
  build) build; echo "built bin/ipeye-tt"; exit 0 ;;
  test)  shift; exec docker run --rm -v "$PWD":/src -w /src -v "$MODCACHE":/go/pkg/mod "$IMAGE" go test ./... "$@" ;;
esac

[ -x bin/ipeye-tt ] || build
# Named, so a killed client never leaves the container behind; --init so a
# signal reaches the process and the verdict is still printed.
NAME=ipeye-tt-$$
trap 'docker kill "$NAME" >/dev/null 2>&1' INT TERM
docker run --rm --init --name "$NAME" --network host -v "$PWD":/src -w /src "$IMAGE" ./bin/ipeye-tt "$@"
st=$?
docker kill "$NAME" >/dev/null 2>&1
exit $st
