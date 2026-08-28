#!/bin/sh
# Example failing job: writes to stderr and exits with a chosen code so the
# failure path (exit_code, success_codes, run status) can be verified.
#   $1: exit code (default 1)
set -eu
code=${1:-1}
echo "about to fail with exit code $code" >&2
exit "$code"
