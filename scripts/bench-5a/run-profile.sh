#!/usr/bin/env bash
# 5A small-server benchmark for minicrond (zero log viewers).
#
# Usage: run-profile.sh <minicrond-binary> <out-dir>
#
# Environment knobs (defaults in brackets):
#   SCENARIOS  space-separated list [idle-empty idle-sleepers active]
#              (also available: active-flush1 = active with worker_flush_interval=1)
#   REPS       repetitions per scenario, run serially and interleaved [2]
#   DURATION   measured window, seconds [180]
#   WARMUP     seconds between /readyz and the window [20]
#   STRACE     seconds of strace -c syscall counting after the window, 0=off [30]
#   DAEMON_CPU CPU the daemon is pinned to [0]; CHILD_CPUS children [1-3]
#   SAMPLER_CPU CPU for the sampler/strace [3]
#   PPROF_ADDR  host:port; only for a binary built with the benchmark-only
#              pprof hook (MINICRON_BENCH_PPROF). Also sets GODEBUG=gctrace=1
#              and collects CPU/alloc/heap profiles during the window. []
#   STRACE_DETAIL seconds of per-call strace (-y -T) of write/pwrite64/fsync/
#              nanosleep after the counting sample, for attribution; 0=off [0]
#
# The daemon runs with GOMAXPROCS=1 under taskset. Children re-pin themselves
# to CHILD_CPUS so their work does not share the daemon's CPU. Only the
# daemon PID is measured. Nothing reads logs during the run (zero viewers);
# /api/v1/monitor is called exactly twice (window start and end) for
# goroutine/heap counts.
set -euo pipefail

BIN=$(readlink -f "${1:?usage: run-profile.sh <minicrond-binary> <out-dir>}")
OUT=$(mkdir -p "${2:?out-dir}" && readlink -f "$2")
HERE=$(cd "$(dirname "$0")" && pwd)
WL="$HERE/wl.sh"
SCENARIOS=${SCENARIOS:-"idle-empty idle-sleepers active"}
REPS=${REPS:-2}
DURATION=${DURATION:-180}
WARMUP=${WARMUP:-20}
STRACE=${STRACE:-30}
DAEMON_CPU=${DAEMON_CPU:-0}
CHILD_CPUS=${CHILD_CPUS:-1-3}
SAMPLER_CPU=${SAMPLER_CPU:-3}
PPROF_ADDR=${PPROF_ADDR:-}
STRACE_DETAIL=${STRACE_DETAIL:-0}

[ -x "$BIN" ] || { echo "not executable: $BIN" >&2; exit 1; }
chmod +x "$WL"

worker() { # name kind [extra toml lines...]
  local name=$1 kind=$2; shift 2
  printf '\n[[worker]]\nname = "%s"\ncommand = "exec /usr/bin/taskset -c %s /bin/sh %s %s %s"\nrestart = "always"\n' \
    "$name" "$CHILD_CPUS" "$WL" "$kind" "$EV"
  for l in "$@"; do printf '%s\n' "$l"; done
}
job() { # name schedule kind
  printf '\n[[job]]\nname = "%s"\nschedule = "%s"\ncommand = "exec /usr/bin/taskset -c %s /bin/sh %s %s %s"\non_overlap = "skip"\n' \
    "$1" "$2" "$CHILD_CPUS" "$WL" "$3" "$EV"
}

# tier_defs <sleepers> <periodic> <bursty> <chatty> <crash> <crash-delay> <jobgroup>...
# jobgroup = "<count> <every>"; job kinds rotate lines/burst/silent/mixed/tick.
tier_defs() {
  local sl=$1 pe=$2 bu=$3 ch=$4 cr=$5 cd=$6; shift 6
  local i
  for i in $(seq 1 "$sl"); do worker "sleep-$i" sleeper; done
  for i in $(seq 1 "$pe"); do worker "periodic-$i" periodic; done
  for i in $(seq 1 "$bu"); do worker "bursty-$i" bursty; done
  for i in $(seq 1 "$ch"); do worker "chatty-$i" chatty; done
  for i in $(seq 1 "$cr"); do worker "crash-$i" crashloop "restart_delay = $cd" 'max_restart_attempts = 1000'; done
  local kinds=(job-lines job-burst job-silent job-mixed job-tick) g n every k=0
  for g in "$@"; do
    n=${g% *}; every=${g#* }
    for i in $(seq 1 "$n"); do job "j-$every-$i" "@every $every" "${kinds[$((k % 5))]}"; k=$((k+1)); done
  done
}

write_config() { # scenario config-path
  local scen=$1 cfg=$2
  MINICRON_CONFIG="$cfg" "$BIN" init >/dev/null
  sed -i 's/^tcp_enabled = true/tcp_enabled = false/' "$cfg"
  if [ "$scen" = active-flush1 ]; then
    sed -i 's/^worker_flush_interval = 15.*/worker_flush_interval = 1 # minutes (bench variant)/' "$cfg"
  fi
  case "$scen" in
  idle-empty) ;;
  tier-tiny)    # hobby VPS: 1 worker, 3 once-a-minute jobs
    {
      worker w-1 periodic
      job t-lines "@every 1m" job-lines
      job t-burst "@every 1m" job-burst
      job t-tick "@every 1m" job-tick
    } >>"$cfg" ;;
  tier-normal)  # 25 workers, 60 jobs at 10 s / 30 s / 1 min cadence
    sed -i 's/^max_concurrent_runs = .*/max_concurrent_runs = 64/' "$cfg"
    tier_defs 10 8 3 2 2 5 "20 10s" "20 30s" "20 1m" >>"$cfg" ;;
  tier-large)   # 100 workers, 300 jobs at 5 s / 15 s / 1 min cadence
    sed -i 's/^max_concurrent_runs = .*/max_concurrent_runs = 256/' "$cfg"
    tier_defs 40 30 10 10 10 5 "100 5s" "100 15s" "100 1m" >>"$cfg" ;;
  idle-sleepers)
    for i in $(seq 1 10); do worker "sleep-$i" sleeper; done >>"$cfg" ;;
  active|active-flush1)
    {
      worker sleep-1 sleeper
      worker sleep-2 sleeper
      worker crash-1 crashloop 'restart_delay = 2' 'max_restart_attempts = 1000'
      for i in 1 2 3 4; do worker "periodic-$i" periodic; done
      worker bursty-1 bursty
      worker bursty-2 bursty
      worker chatty-1 chatty
      job job-lines "@every 5s" job-lines
      job job-burst "@every 5s" job-burst
      job job-silent "@every 5s" job-silent
      job job-mixed "@every 5s" job-mixed
      job tick-echo "@every 1s" job-tick
      printf '\n[[job]]\nname = "tick-true"\nschedule = "@every 1s"\ncommand = "exec /usr/bin/taskset -c %s /bin/true"\n' "$CHILD_CPUS"
    } >>"$cfg" ;;
  *) echo "unknown scenario $scen" >&2; exit 1 ;;
  esac
  "$BIN" validate "$cfg" >/dev/null
}

api() { curl -sS --max-time 5 --unix-socket "$DATA/minicron.sock" "http://minicron$1"; }

run_one() { # scenario rep
  local scen=$1 rep=$2 dir="$OUT/$scen-r$rep"
  rm -rf "$dir"; mkdir -p "$dir/ev"; mkdir -m 0700 "$dir/data"
  EV="$dir/ev"; DATA="$dir/data"; local cfg="$dir/minicron.toml"
  write_config "$scen" "$cfg"
  echo "== $scen rep $rep -> $dir"
  local extra=()
  if [ -n "$PPROF_ADDR" ]; then extra=(MINICRON_BENCH_PPROF="$PPROF_ADDR" GODEBUG=gctrace=1); fi
  local dcpu=$DAEMON_CPU procs=1
  case "$scen" in tier-normal|tier-large) dcpu=${TIER_DAEMON_CPU:-0-1}; procs=2 ;; esac
  env MINICRON_CONFIG="$cfg" MINICRON_DATA="$DATA" GOMAXPROCS=$procs "${extra[@]}" \
    taskset -c "$dcpu" "$BIN" daemon >"$dir/daemon.log" 2>&1 &
  local pid=$!
  echo "$pid" >"$dir/pid"
  local t_start; t_start=$(date +%s.%N)
  for _ in $(seq 1 200); do
    api /readyz >/dev/null 2>&1 && break
    kill -0 "$pid" 2>/dev/null || { echo "daemon died; see $dir/daemon.log" >&2; cat "$dir/daemon.log" >&2; exit 1; }
    sleep 0.05
  done
  local t_ready; t_ready=$(date +%s.%N)
  sleep "$WARMUP"
  api /api/v1/monitor >"$dir/monitor-start.json" || true
  local w0; w0=$(date +%s.%N)
  du -sB1 "$DATA" | cut -f1 >"$dir/du-start"
  ( cd "$DATA" && find . -type f -printf '%P %s\n' 2>/dev/null | sort || true ) >"$dir/files-start"
  local prof=""
  if [ -n "$PPROF_ADDR" ]; then
    ( sleep 5; taskset -c "$SAMPLER_CPU" "$HERE/profile-collect.sh" "$PPROF_ADDR" "$dir/pprof" $((DURATION - 15)) ) &
    prof=$!
  fi
  taskset -c "$SAMPLER_CPU" python3 "$HERE/sampler.py" "$pid" "$DURATION" 1 "$dir/samples.csv" "$DATA"
  [ -n "$prof" ] && wait "$prof"
  local w1; w1=$(date +%s.%N)
  du -sB1 "$DATA" | cut -f1 >"$dir/du-end"
  ( cd "$DATA" && find . -type f -printf '%P %s\n' 2>/dev/null | sort || true ) >"$dir/files-end"
  api /api/v1/monitor >"$dir/monitor-end.json" || true
  if [ "$STRACE" -gt 0 ]; then
    # Attach to the daemon's existing threads only (no -f), so forked
    # children are not traced. Threads created during the window are missed.
    local args=(); for t in /proc/"$pid"/task/*; do args+=(-p "${t##*/}"); done
    ls /proc/"$pid"/task | wc -l >"$dir/strace-threads-before"
    taskset -c "$SAMPLER_CPU" timeout -s INT "$STRACE" strace -c -o "$dir/strace.txt" "${args[@]}" 2>/dev/null || true
    ls /proc/"$pid"/task | wc -l >"$dir/strace-threads-after"
  fi
  if [ "$STRACE_DETAIL" -gt 0 ]; then
    local args=(); for t in /proc/"$pid"/task/*; do args+=(-p "${t##*/}"); done
    taskset -c "$SAMPLER_CPU" timeout -s INT "$STRACE_DETAIL" strace -y -T -tt -e trace=write,pwrite64,fsync,fdatasync,nanosleep,fcntl -o "$dir/strace-detail.txt" "${args[@]}" 2>/dev/null || true
  fi
  local s0; s0=$(date +%s.%N)
  kill -TERM "$pid"; wait "$pid" || true
  local s1; s1=$(date +%s.%N)
  cat >"$dir/meta.env" <<EOF
scenario=$scen
rep=$rep
binary=$BIN
t_start=$t_start
t_ready=$t_ready
window_start=$w0
window_end=$w1
strace_s=$STRACE
strace_detail_s=$STRACE_DETAIL
pprof=$PPROF_ADDR
shutdown_s=$(python3 -c "print(f'{$s1-$s0:.3f}')")
EOF
  du -sB1 "$DATA" | cut -f1 >"$dir/du-final"
  python3 "$HERE/analyze.py" "$dir" >"$dir/summary.json"
}

{
  echo "binary: $BIN"; "$BIN" version
  echo "date: $(date -Is)"; uname -a; nproc
  grep -m1 'model name' /proc/cpuinfo; df -T "$OUT" | tail -1
  echo "scenarios=$SCENARIOS reps=$REPS duration=$DURATION warmup=$WARMUP strace=$STRACE"
} >>"$OUT/environment.txt"

for rep in $(seq 1 "$REPS"); do
  for scen in $SCENARIOS; do run_one "$scen" "$rep"; done
done
python3 "$HERE/summarize.py" "$OUT" | tee "$OUT/summary.md"
