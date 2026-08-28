#!/bin/sh
# Example worker: runs forever, printing a timestamped line every N seconds.
# Demonstrates supervisor-managed workers (autostart, restart, stop/start).
#   $1: interval seconds (default 5)
set -eu
interval=${1:-5}
trap 'echo "worker stopping"; exit 0' TERM INT
echo "worker started (pid $$)"
while :; do
	sleep "$interval" &
	wait $!
	echo "tick $(date -u +%Y-%m-%dT%H:%M:%SZ)"
done
