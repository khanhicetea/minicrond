#!/bin/sh
# Example job: greet and show the metadata minicron injects into every run.
#   stdin:  none
#   stdout: greeting + injected env + utc timestamp
#   exit:   0
set -eu
say=${1:-hello}
echo "hello from minicron example: $say"
echo "job=$MINICRON_JOB run_id=$MINICRON_RUN_ID trigger=$MINICRON_TRIGGER attempt=$MINICRON_ATTEMPT"
echo "utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
