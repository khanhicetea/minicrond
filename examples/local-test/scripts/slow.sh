#!/bin/sh
# Example long-running job: sleeps for N seconds while printing progress.
# Used with job timeout/grace settings to demonstrate process-group kill,
# and can be stopped mid-run via the API (POST /api/v1/runs/{id}/stop).
#   $1: seconds to sleep (default 60)
set -eu
secs=${1:-60}
trap 'echo "got stop signal, exiting"; exit 143' TERM INT
i=0
while [ "$i" -lt "$secs" ]; do
	echo "sleeping... $i/$secs"
	i=$((i + 1))
	sleep 1
done
echo "finished sleeping"
