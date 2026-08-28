#!/bin/sh
# Example job: emit a few timestamped heartbeat lines (demonstrates tagged
# log capture of stdout, one line per second).
#   $1: number of lines (default 3)
set -eu
count=${1:-3}
i=1
while [ "$i" -le "$count" ]; do
	echo "heartbeat $i/$count at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	i=$((i + 1))
	[ "$i" -le "$count" ] && sleep 1
done
