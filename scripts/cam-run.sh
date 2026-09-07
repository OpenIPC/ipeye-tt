#!/bin/sh
# Run a majestic binary on a camera from /tmp, leaving /usr/bin/majestic alone.
#
#   scripts/cam-run.sh <camera-host> <majestic-binary>
#   scripts/cam-run.sh <camera-host> stock        # back to the installed service
#
# Set CAM_ENV to prefix the start command (e.g. CAM_ENV="SENSOR=imx335").
# The stock service is stopped first (majestic refuses to start beside
# another instance); cam-restore.sh or `cam-run.sh HOST stock` brings it
# back. Prints the version string of what is now running.
set -e
HOST=${1:?usage: cam-run.sh <camera-host> <binary|stock>}
BIN=${2:?usage: cam-run.sh <camera-host> <binary|stock>}
DIR=/tmp/majestic-ipeye
SSH="ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"

stop_all() {
  $SSH "root@$HOST" '/etc/init.d/S95majestic stop >/dev/null 2>&1; killall -q majestic; n=0; while pidof majestic >/dev/null && [ $n -lt 20 ]; do sleep 1; n=$((n+1)); done; ! pidof majestic >/dev/null'
}

if [ "$BIN" = stock ]; then
  stop_all
  $SSH "root@$HOST" '/etc/init.d/S95majestic start >/dev/null 2>&1; sleep 2; pidof majestic >/dev/null && majestic -v'
  exit 0
fi

[ -f "$BIN" ] || { echo "no such binary: $BIN" >&2; exit 1; }
$SSH "root@$HOST" "mkdir -p $DIR"
scp -O -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "$BIN" "root@$HOST:$DIR/majestic.new"
stop_all
$SSH "root@$HOST" "cd $DIR && mv majestic.new majestic && chmod +x majestic && : > majestic.log && ($CAM_ENV ./majestic -s >/dev/null 2>&1 &) ; sleep 2
  for p in \$(pidof majestic); do [ \"\$(readlink -f /proc/\$p/exe)\" = $DIR/majestic ] && echo \"running pid \$p: \$($DIR/majestic -v)\"; done"
