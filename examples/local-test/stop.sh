#!/bin/sh
# Stop the daemon started by start.sh (SIGTERM, then SIGKILL after 10s).
set -eu

. "$(dirname -- "$0")/common.sh"

load_env

if ! pid=$(daemon_pid); then
	echo "daemon is not running"
	rm -f "$MINICRON_DATA/minicron.sock"
	exit 0
fi

echo "==> stopping daemon (pid $pid)"
kill -TERM "$pid" 2>/dev/null || true

i=0
while [ "$i" -lt 10 ] && kill -0 "$pid" 2>/dev/null; do
	i=$((i + 1))
	sleep 1
done

if kill -0 "$pid" 2>/dev/null; then
	echo "did not exit after 10s, sending SIGKILL" >&2
	kill -KILL "$pid" 2>/dev/null || true
else
	echo "stopped cleanly"
fi
rm -f "$MINICRON_DATA/minicron.sock"
