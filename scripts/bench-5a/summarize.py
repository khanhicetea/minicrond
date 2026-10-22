#!/usr/bin/env python3
"""Summarize run-profile.sh output: median [min–max] across reps per scenario.

Usage: summarize.py <out-dir> [<out-dir-B>]   (two dirs: A/B side by side)
"""
import glob, json, os, statistics, sys

METRICS = [
    ("CPU ms/s (stat utime+stime)", lambda s: s["cpu_ms_s"]),
    ("  user ms/s", lambda s: s["utime_ms_s"]),
    ("  sys ms/s", lambda s: s["stime_ms_s"]),
    ("CPU ms/s (schedstat)", lambda s: s["sched_cpu_ms_s"]),
    ("CPU ms per 1 s, p95", lambda s: s["cpu_ms_per_1s"]["p95"]),
    ("CPU ms per 1 s, max", lambda s: s["cpu_ms_per_1s"]["max"]),
    ("context switches/s", lambda s: s["ctxsw_s"]),
    ("RSS median MiB", lambda s: s["rss_mib"]["median"]),
    ("RSS max MiB", lambda s: s["rss_mib"]["max"]),
    ("VmHWM MiB", lambda s: s["hwm_mib"]),
    ("threads max", lambda s: s["threads_max"]),
    ("goroutines (end)", lambda s: s.get("goroutines_end")),
    ("Go heap objects MiB (end)", lambda s: s.get("heap_objects_mib_end")),
    ("write_bytes KiB/s", lambda s: round(s["io"]["write_bytes_s"] / 1024, 2)),
    ("write syscalls/s (syscw)", lambda s: s["io"]["syscw_s"]),
    ("read syscalls/s (syscr)", lambda s: s["io"]["syscr_s"]),
    ("data dir du growth KiB/min", lambda s: s["disk"]["du_growth_kib_per_min"]),
    ("  durable growth KiB/min (db files + log buffers, excl. WAL)", lambda s: s["disk"]["durable_growth_kib_per_min"]),
    ("    metadata db KiB/min", lambda s: s["disk"]["metadata_db_growth_kib_per_min"]),
    ("    log archive db KiB/min", lambda s: s["disk"]["archive_db_growth_kib_per_min"]),
    ("    live log buffers KiB/min", lambda s: s["disk"]["log_buffer_growth_kib_per_min"]),
    ("data dir du after shutdown MiB", lambda s: round(s["disk"]["du_after_shutdown"] / 2**20, 2)),
    ("fsync/s (strace)", lambda s: s.get("strace_per_s", {}).get("fsync")),
    ("fdatasync/s (strace)", lambda s: s.get("strace_per_s", {}).get("fdatasync")),
    ("write/s (strace)", lambda s: s.get("strace_per_s", {}).get("write")),
    ("read/s (strace)", lambda s: s.get("strace_per_s", {}).get("read")),
    ("futex/s (strace)", lambda s: s.get("strace_per_s", {}).get("futex")),
    ("epoll_pwait/s (strace)", lambda s: s.get("strace_per_s", {}).get("epoll_pwait")),
    ("nanosleep/s (strace)", lambda s: s.get("strace_per_s", {}).get("nanosleep")),
    ("all syscalls/s (strace)", lambda s: s.get("strace_per_s", {}).get("total")),
    ("runs started in window", lambda s: s.get("runs_in_window")),
    ("fire→start p50 ms (1s jobs)", lambda s: (s.get("fire_start_lag_1s_jobs_ms") or {}).get("p50")),
    ("fire→start p95 ms (1s jobs)", lambda s: (s.get("fire_start_lag_1s_jobs_ms") or {}).get("p95")),
    ("fire→start p50 ms (5s jobs)", lambda s: (s.get("fire_start_lag_5s_jobs_ms") or {}).get("p50")),
    ("fire→start p95 ms (5s jobs)", lambda s: (s.get("fire_start_lag_5s_jobs_ms") or {}).get("p95")),
    ("fire→start max ms (all)", lambda s: (s.get("fire_start_lag_ms") or {}).get("max")),
    ("fire→child p50 ms", lambda s: (s.get("fire_child_lag_ms") or {}).get("p50")),
    ("fire→child p95 ms", lambda s: (s.get("fire_child_lag_ms") or {}).get("p95")),
    ("restart overhead p50 ms (child exit→next child start − delay)", lambda s: (s.get("restart_gap_child_ms") or {}).get("p50")),
    ("restart overhead p95 ms", lambda s: (s.get("restart_gap_child_ms") or {}).get("p95")),
    ("restart overhead max ms", lambda s: (s.get("restart_gap_child_ms") or {}).get("max")),
    ("child exit→run ended p50 ms", lambda s: (s.get("exit_to_ended_ms") or {}).get("p50")),
    ("host busy % of 4 CPUs (noise)", lambda s: s["host"]["busy_pct_4cpu"]),
    ("host load1 median (noise)", lambda s: s["host"]["load1_median"]),
    ("shutdown s", lambda s: s["shutdown_s"]),
    ("daemon WARN/ERROR lines", lambda s: s["daemon_log_warn_error"]),
]

def load(root):
    by = {}
    for f in sorted(glob.glob(os.path.join(root, "*", "summary.json"))):
        try:
            s = json.load(open(f))
        except Exception:
            continue
        by.setdefault(s["scenario"], []).append(s)
    return by

def cell(vals):
    vals = [v for v in vals if v is not None]
    if not vals:
        return "–"
    m = statistics.median(vals)
    fmt = lambda x: f"{x:.3g}" if isinstance(x, float) and abs(x) < 100 else f"{x:.0f}" if isinstance(x, float) else str(x)
    if len(vals) == 1:
        return fmt(m)
    return f"{fmt(m)} [{fmt(min(vals))}–{fmt(max(vals))}]"

roots = sys.argv[1:]
data = [load(r) for r in roots]
order = ["idle-empty", "idle-sleepers", "active", "active-flush1"]
scens = [s for s in order if any(s in d for d in data)] + sorted({s for d in data for s in d} - set(order))
hdr = ["metric"]
for s in scens:
    for i, d in enumerate(data):
        n = len(d.get(s, []))
        hdr.append(f"{s}{' '+'AB'[i] if len(data) > 1 else ''} (n={n})")
print("| " + " | ".join(hdr) + " |")
print("|" + "---|" * len(hdr))
for name, fn in METRICS:
    row = [name]
    for s in scens:
        for d in data:
            vals = []
            for x in d.get(s, []):
                try:
                    vals.append(fn(x))
                except (KeyError, TypeError):
                    vals.append(None)
            row.append(cell(vals))
    print("| " + " | ".join(row) + " |")
