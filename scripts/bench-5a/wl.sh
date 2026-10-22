#!/bin/sh
# Workload child for the 5A benchmark. Usage: wl.sh <kind> <event-dir>
# Children are re-pinned off CPU 0 by the caller (taskset -c 1-3) so the
# daemon's CPU 0 is not shared with child work; we measure daemon cost only.
kind=$1
ev=$2
ts() { date +%s.%N; }
pad="pad-0123456789abcdefghijklmnopqrstuvwxyz-ABCDEFGHIJKLMNOPQRSTUVWXYZ"
case "$kind" in
sleeper)
  exec sleep infinity ;;
periodic)       # one ~100 B line per second, forever
  n=0
  while :; do n=$((n+1)); echo "periodic ${MINICRON_JOB} line=$n $(date +%T) $pad"; sleep 1; done ;;
bursty)         # 500 lines every 10 s
  n=0
  while :; do
    n=$((n+1)); i=0
    while [ $i -lt 500 ]; do i=$((i+1)); echo "burst=$n line=$i ${MINICRON_JOB} $pad"; done
    sleep 10
  done ;;
chatty)         # ~200 lines/s: 20 lines every 100 ms
  n=0
  while :; do
    i=0
    while [ $i -lt 20 ]; do i=$((i+1)); n=$((n+1)); echo "chatty seq=$n ${MINICRON_JOB} $pad"; done
    sleep 0.1
  done ;;
crashloop)      # restart-latency probe: run 5 s, exit 1
  echo "$(ts) start ${MINICRON_RUN_ID}" >> "$ev/crash.events"
  echo "crashloop starting attempt ${MINICRON_ATTEMPT}"
  sleep 5
  echo "crashloop exiting 1" >&2
  echo "$(ts) exit ${MINICRON_RUN_ID}" >> "$ev/crash.events"
  exit 1 ;;
job-lines)      # 4 s, one line per second
  echo "$(ts) start ${MINICRON_RUN_ID} ${MINICRON_JOB}" >> "$ev/jobs.events"
  for i in 1 2 3 4; do echo "job ${MINICRON_JOB} step $i $pad"; sleep 1; done ;;
job-burst)      # 100 lines at once, then 3 s
  echo "$(ts) start ${MINICRON_RUN_ID} ${MINICRON_JOB}" >> "$ev/jobs.events"
  i=0; while [ $i -lt 100 ]; do i=$((i+1)); echo "job ${MINICRON_JOB} row $i $pad"; done
  sleep 3 ;;
job-silent)     # 3.5 s, no output
  echo "$(ts) start ${MINICRON_RUN_ID} ${MINICRON_JOB}" >> "$ev/jobs.events"
  sleep 3.5 ;;
job-mixed)      # 4 s, 10 lines/s on stdout plus a stderr line per second
  echo "$(ts) start ${MINICRON_RUN_ID} ${MINICRON_JOB}" >> "$ev/jobs.events"
  for s in 1 2 3 4; do
    for i in 1 2 3 4 5 6 7 8 9 10; do echo "job ${MINICRON_JOB} s=$s i=$i $pad"; done
    echo "warn ${MINICRON_JOB} s=$s" >&2
    sleep 1
  done ;;
job-tick)       # exits immediately with one line
  echo "$(ts) start ${MINICRON_RUN_ID} ${MINICRON_JOB}" >> "$ev/jobs.events"
  echo "tick ${MINICRON_JOB}" ;;
*) echo "unknown kind $kind" >&2; exit 2 ;;
esac
