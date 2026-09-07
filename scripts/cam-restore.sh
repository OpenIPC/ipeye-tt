#!/bin/sh
# Undo cam-setup.sh and cam-run.sh: stop the test binary, put the backed-up
# files back, start the installed service.
#
#   scripts/cam-restore.sh <camera-host>
set -e
HOST=${1:?usage: cam-restore.sh <camera-host>}
SSH="ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
$SSH "root@$HOST" '/etc/init.d/S95majestic stop >/dev/null 2>&1; killall -q majestic; sleep 1
  for f in majestic.yaml hosts ca-certificates.crt; do
    case $f in ca-certificates.crt) d=/etc/ssl/certs/$f;; *) d=/etc/$f;; esac
    [ -f /root/ipeye-tt.bak/$f ] && cp -p /root/ipeye-tt.bak/$f $d && echo "restored $d"
  done
  rm -rf /tmp/majestic-ipeye
  /etc/init.d/S95majestic start >/dev/null 2>&1; sleep 2; pidof majestic >/dev/null && majestic -v'
