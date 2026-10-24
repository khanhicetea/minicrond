#!/usr/bin/env bash
# Real-filesystem storage-fault test for ADR-16 (execution first).
#
# Usage: run.sh <minicrond-binary> <out-dir>     (needs sudo, mkfs.ext4, python3)
#
# The data directory lives on a small ext4 image (-m 0). A scheduled job and a
# crash-looping worker write liveness markers to <out-dir>, which is NOT on the
# image, so execution is observed independently of the storage under test.
# Phases: healthy -> disk full -> space freed -> SQLite write lock held by
# another process -> boot on a full disk -> filesystem shut down
# (EXT4_IOC_SHUTDOWN, every I/O fails) -> stop. Each phase reports how many
# job runs and worker starts happened and the daemon's run_history counters.
set -euo pipefail
BIN=$(readlink -f "${1:?usage: run.sh <minicrond-binary> <out-dir>}")
OUT=$(mkdir -p "${2:?out-dir}" && readlink -f "$2")
PHASE=${PHASE_SECONDS:-20}
WORK=$(mktemp -d /tmp/mcfault.XXXX)   # short path: the Unix socket lives in the data dir
IMG=$WORK/fs.img MNT=$WORK/m DATA=$WORK/m/data
cleanup() {
  [[ -n "${PID:-}" ]] && kill -KILL "$PID" 2>/dev/null || true
  sudo umount -l "$MNT" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT
truncate -s 96M "$IMG"
/sbin/mkfs.ext4 -q -F -m 0 "$IMG"
mkdir -p "$MNT"
sudo mount -o loop "$IMG" "$MNT"
sudo install -d -o "$(id -u)" -g "$(id -g)" -m 0700 "$DATA"
: > "$OUT/job.ticks"; : > "$OUT/worker.starts"
cat > "$WORK/minicron.toml" <<TOML
[server]
tcp_enabled = false
[scheduler]
shutdown_grace = 3
[logs]
disk_min_free = 0
[[job]]
name = "tick"
schedule = "@every 1s"
command = "date +%s >> $OUT/job.ticks; head -c 2000 /dev/urandom | base64"
[[worker]]
name = "flaky"
command = "date +%s >> $OUT/worker.starts; sleep 2; exit 1"
restart = "always"
restart_delay = 1
healthy_after = 1
TOML
export MINICRON_CONFIG=$WORK/minicron.toml MINICRON_DATA=$DATA
start() { "$BIN" daemon >> "$OUT/daemon.log" 2>&1 & PID=$!; sleep 3; }
count() { wc -l < "$1" | tr -d ' '; }
status() { timeout 5 "$BIN" status 2>/dev/null | python3 -c 'import json,sys; d=json.load(sys.stdin)["diagnostics"]["run_history"]; print(" ".join(f"{k}={v}" for k,v in sorted(d.items())))' 2>/dev/null || echo "status unavailable"; }
phase() { # name seconds
  local j0 w0; j0=$(count "$OUT/job.ticks"); w0=$(count "$OUT/worker.starts")
  sleep "$2"
  printf '%-22s %3ss  job runs %3s  worker starts %3s  daemon %s  %s\n' "$1" "$2" \
    $(( $(count "$OUT/job.ticks") - j0 )) $(( $(count "$OUT/worker.starts") - w0 )) \
    "$(kill -0 "$PID" 2>/dev/null && echo alive || echo DEAD)" "$(status)" | tee -a "$OUT/summary.txt"
}
fill() { fallocate -l "$(( $(stat -f -c '%a*%S' "$MNT") ))" "$MNT/data/filler" 2>/dev/null || dd if=/dev/zero of="$MNT/data/filler" bs=1M 2>/dev/null || true; }
rows() { python3 - "$DATA/minicron.db" <<'PY'
import sqlite3,sys
db=sqlite3.connect(f"file:{sys.argv[1]}?mode=ro",uri=True)
print(" ".join(f"{j}:{n}" for j,n in db.execute("select job,count(*) from runs group by job")))
PY
}
: > "$OUT/summary.txt"
start
phase healthy "$PHASE"
fill; echo "free after fill: $(df -B1 --output=avail "$MNT" | tail -1) B" | tee -a "$OUT/summary.txt"
phase disk-full "$PHASE"
rm -f "$MNT/data/filler"
phase space-freed "$PHASE"
echo "rows after recovery: $(rows)" | tee -a "$OUT/summary.txt"
python3 - "$DATA/minicron.db" "$PHASE" <<'PY' &
import sqlite3,sys,time
db=sqlite3.connect(sys.argv[1],isolation_level=None,timeout=30)
db.execute("BEGIN EXCLUSIVE"); time.sleep(int(sys.argv[2])+2); db.execute("COMMIT")
PY
LOCKER=$!; sleep 1
phase db-write-locked "$PHASE"
wait $LOCKER || true
phase lock-released "$PHASE"
t0=$(date +%s.%N); kill -TERM "$PID"; wait "$PID" || true; t1=$(date +%s.%N)
echo "shutdown (healthy) $(python3 -c "print(round($t1-$t0,2))") s" | tee -a "$OUT/summary.txt"
fill
start
phase boot-on-full-disk "$PHASE"
rm -f "$MNT/data/filler"
# EXT4_IOC_SHUTDOWN (_IOR('X', 125, __u32)) with EXT4_GOING_FLAGS_NOLOGFLUSH:
# every later I/O on the filesystem fails with EIO.
sudo python3 -c 'import fcntl,os,struct,sys; fd=os.open(sys.argv[1],os.O_RDONLY); fcntl.ioctl(fd,0x8004587D,struct.pack("I",2))' "$MNT"
phase fs-shutdown "$PHASE"
t0=$(date +%s.%N); kill -TERM "$PID"; wait "$PID" || true; t1=$(date +%s.%N)
echo "shutdown (dead fs) $(python3 -c "print(round($t1-$t0,2))") s" | tee -a "$OUT/summary.txt"
PID=
"$BIN" daemon >> "$OUT/daemon.log" 2>&1 && echo "boot on dead fs: started (unexpected)" | tee -a "$OUT/summary.txt" || echo "boot on dead fs: exit $? (expected: the metadata database is unreadable)" | tee -a "$OUT/summary.txt"
grep -E "level=(WARN|ERROR)| WARN | ERROR " "$OUT/daemon.log" | sed -E 's/^[0-9/]+ [0-9:]+ //; s/run=[0-9a-f-]+/run=…/g; s/(error=").{0,80}.*/\1…/' | sort | uniq -c | sort -rn > "$OUT/log-summary.txt" || true
