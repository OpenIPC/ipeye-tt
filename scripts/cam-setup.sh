#!/bin/sh
# Point an OpenIPC camera at this harness instead of the real cloud.
#
#   scripts/cam-setup.sh <camera-host> <harness-ip> [certs/ca.crt]
#
# Everything it touches is backed up once under /root/ipeye-tt.bak on the
# camera and put back by cam-restore.sh:
#   /etc/hosts                          the cloud's name → the harness
#   /etc/ssl/certs/ca-certificates.crt  the harness CA appended, so the
#                                       camera's TLS verification passes
#   /etc/majestic.yaml                  cloud connector on, audio on
set -e
HOST=${1:?usage: cam-setup.sh <camera-host> <harness-ip> [ca.crt]}
IP=${2:?usage: cam-setup.sh <camera-host> <harness-ip> [ca.crt]}
CA=${3:-$(dirname "$0")/../certs/ca.crt}
NAME=${BALANCER_NAME:-api.ipeye.ru}
SSH="ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"

[ -f "$CA" ] || { echo "no CA at $CA — run the harness once in reverse mode to mint it" >&2; exit 1; }
FP=$(sed -n 2p "$CA")

$SSH "root@$HOST" 'mkdir -p /root/ipeye-tt.bak; for f in /etc/majestic.yaml /etc/hosts /etc/ssl/certs/ca-certificates.crt; do [ -f /root/ipeye-tt.bak/$(basename $f) ] || cp -p $f /root/ipeye-tt.bak/; done'
$SSH "root@$HOST" "cat > /root/ipeye-tt.bak/ipeye-tt-ca.crt" < "$CA"
$SSH "root@$HOST" "grep -qF '$FP' /etc/ssl/certs/ca-certificates.crt || cat /root/ipeye-tt.bak/ipeye-tt-ca.crt >> /etc/ssl/certs/ca-certificates.crt
  sed -i '/ $NAME\$/d' /etc/hosts; echo '$IP $NAME' >> /etc/hosts
  yaml-cli -i /etc/majestic.yaml -s .ipeye.enabled true >/dev/null
  yaml-cli -i /etc/majestic.yaml -s .audio.enabled true >/dev/null
  echo '--- hosts:'; grep $NAME /etc/hosts
  echo '--- config:'; yaml-cli -i /etc/majestic.yaml -g .ipeye.enabled; yaml-cli -i /etc/majestic.yaml -g .audio.enabled; yaml-cli -i /etc/majestic.yaml -g .audio.srate || true
  echo '--- CA in bundle:'; grep -c -- '-----BEGIN' /etc/ssl/certs/ca-certificates.crt"
