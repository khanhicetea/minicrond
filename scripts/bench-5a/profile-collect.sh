#!/usr/bin/env bash
# Collect pprof data from a daemon built with the benchmark-only pprof hook
# (MINICRON_BENCH_PPROF). Usage: profile-collect.sh <host:port> <out-dir> <seconds>
set -u
A=$1; D=$2; S=$3
mkdir -p "$D"
g() { curl -sS --max-time $((S + 30)) -o "$D/$2" "http://$A/debug/pprof/$1"; }
g "heap?gc=1" heap-start.pb.gz
g "heap?debug=1" memstats-start.txt
g "allocs?seconds=$S" allocs-delta.pb.gz &
g "profile?seconds=$S" cpu.pb.gz &
wait
g "heap?gc=1" heap-end.pb.gz
g "heap?debug=1" memstats-end.txt
g "goroutine?debug=1" goroutines.txt
g "mutex" mutex.pb.gz
g "block" block.pb.gz
