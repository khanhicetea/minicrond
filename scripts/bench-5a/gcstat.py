#!/usr/bin/env python3
"""Summarize GODEBUG=gctrace=1 lines in <run-dir>/daemon.log for the window.

gctrace format: gc N @T s P%: a+b+c ms clock, d+e/f/g+h ms cpu, X->Y->Z MB, G MB goal, ...
Z (live heap after GC) is the marked heap; cpu = d+e+f+g+h (STW + assist + bg + idle).
"""
import json, os, re, statistics, sys

d = sys.argv[1]
meta = dict(l.strip().split("=", 1) for l in open(os.path.join(d, "meta.env")) if "=" in l)
t0 = float(meta["window_start"]) - float(meta["t_start"])
t1 = float(meta["window_end"]) - float(meta["t_start"])
rx = re.compile(r"gc (\d+) @([\d.]+)s \d+%: [\d.+]+ ms clock, ([\d.+/]+) ms cpu, (\d+)->(\d+)->(\d+) MB, (\d+) MB goal")
gcs = []
for line in open(os.path.join(d, "daemon.log")):
    m = rx.search(line)
    if not m:
        continue
    at = float(m.group(2))
    cpu = sum(float(x) for x in re.split(r"[+/]", m.group(3)))
    gcs.append((at, cpu, int(m.group(4)), int(m.group(6)), int(m.group(7))))
win = [g for g in gcs if t0 <= g[0] <= t1]
span = t1 - t0
out = {"gc_total": len(gcs), "gc_in_window": len(win),
       "gc_per_min": round(len(win) / span * 60, 2) if span > 0 else None,
       "gc_cpu_ms_per_s": round(sum(g[1] for g in win) / span, 3) if win else 0,
       "live_after_gc_mb_median": statistics.median(g[3] for g in win) if win else None,
       "heap_goal_mb_median": statistics.median(g[4] for g in win) if win else None}
print(json.dumps(out))
