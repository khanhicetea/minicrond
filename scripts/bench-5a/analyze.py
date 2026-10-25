#!/usr/bin/env python3
"""Analyze one run directory produced by run-profile.sh; prints JSON."""
import csv, json, os, re, shutil, sqlite3, statistics, sys, tempfile

d = sys.argv[1]
meta = dict(l.strip().split("=", 1) for l in open(os.path.join(d, "meta.env")) if "=" in l)
tck = os.sysconf("SC_CLK_TCK")

def pct(xs, p):
    if not xs:
        return None
    xs = sorted(xs)
    k = (len(xs) - 1) * p / 100
    f = int(k); c = min(f + 1, len(xs) - 1)
    return xs[f] + (xs[c] - xs[f]) * (k - f)

def dist(xs, scale=1.0, nd=2):
    if not xs:
        return None
    xs = [x * scale for x in xs]
    return {"n": len(xs), "p50": round(pct(xs, 50), nd), "p95": round(pct(xs, 95), nd),
            "max": round(max(xs), nd), "min": round(min(xs), nd)}

rows = list(csv.DictReader(open(os.path.join(d, "samples.csv"))))
num = lambda r, k: int(r[k])
a, b = rows[0], rows[-1]
win = float(b["t"]) - float(a["t"])
out = {"scenario": meta["scenario"], "rep": int(meta["rep"]), "window_s": round(win, 1)}
dt = lambda k: num(b, k) - num(a, k)
out["cpu_ms_s"] = round((dt("utime") + dt("stime")) * 1000 / tck / win, 3)
out["utime_ms_s"] = round(dt("utime") * 1000 / tck / win, 3)
out["stime_ms_s"] = round(dt("stime") * 1000 / tck / win, 3)
out["sched_cpu_ms_s"] = round(dt("run_ns") / 1e6 / win, 3)
per_s = []
for p, q in zip(rows, rows[1:]):
    span = float(q["t"]) - float(p["t"])
    if span > 0:
        per_s.append((num(q, "run_ns") - num(p, "run_ns")) / 1e6 / span)
out["cpu_ms_per_1s"] = dist(per_s)
out["ctxsw_s"] = round((dt("vcsw") + dt("ivcsw")) / win, 1)
out["nonvol_ctxsw_s"] = round(dt("ivcsw") / win, 2)
out["timeslices_s"] = round(dt("slices") / win, 1)
out["sched_wait_ms_s"] = round(dt("wait_ns") / 1e6 / win, 3)
rss = [num(r, "rss_kb") for r in rows]
out["rss_mib"] = {"start": round(rss[0] / 1024, 2), "median": round(statistics.median(rss) / 1024, 2),
                  "max": round(max(rss) / 1024, 2), "end": round(rss[-1] / 1024, 2)}
out["hwm_mib"] = round(num(b, "hwm_kb") / 1024, 2)
out["threads_max"] = max(num(r, "threads") for r in rows)
out["io"] = {"write_bytes_s": round(dt("write_bytes") / win, 1), "wchar_s": round(dt("wchar") / win, 1),
             "syscw_s": round(dt("syscw") / win, 2), "syscr_s": round(dt("syscr") / win, 2),
             "read_bytes_s": round(dt("read_bytes") / win, 1), "rchar_s": round(dt("rchar") / win, 1)}
if "host_total" in a and a.get("host_total"):
    ht = num(b, "host_total") - num(a, "host_total")
    out["host"] = {"busy_pct_4cpu": round(100 * (num(b, "host_busy") - num(a, "host_busy")) / ht, 1),
                   "steal_pct": round(100 * (num(b, "host_steal") - num(a, "host_steal")) / ht, 2),
                   "iowait_pct": round(100 * (num(b, "host_iowait") - num(a, "host_iowait")) / ht, 2),
                   "load1_max": max(float(r["load1"]) for r in rows),
                   "load1_median": statistics.median(float(r["load1"]) for r in rows)}
if a.get("blk_flush"):
    out["blk"] = {"flush_s": round((num(b, "blk_flush") - num(a, "blk_flush")) / win, 1),
                  "write_ios_s": round((num(b, "blk_wr_ios") - num(a, "blk_wr_ios")) / win, 1)}
ds = [(float(r["t"]), int(r["data_bytes"])) for r in rows if r["data_bytes"]]
du0 = int(open(os.path.join(d, "du-start")).read()); du1 = int(open(os.path.join(d, "du-end")).read())
out["disk"] = {"du_growth_bytes": du1 - du0, "du_end_bytes": du1,
               "du_growth_kib_per_min": round((du1 - du0) / 1024 / (win / 60), 1),
               "apparent_growth_bytes": (ds[-1][1] - ds[0][1]) if len(ds) > 1 else None,
               "du_after_shutdown": int(open(os.path.join(d, "du-final")).read())}

def files(name):
    m = {}
    for l in open(os.path.join(d, name)):
        p, _, s = l.rstrip("\n").rpartition(" ")
        m[p] = int(s)
    return m
f0, f1 = files("files-start"), files("files-end")
def group(p):
    base = os.path.basename(p)
    if base.startswith("minicron-logs.db"):
        return base
    if base.startswith("minicron.db"):
        return base
    return "log-buffers/other"
g = {}
for p in set(f0) | set(f1):
    k = group(p)
    g.setdefault(k, [0, 0])
    g[k][0] += f0.get(p, 0); g[k][1] += f1.get(p, 0)
out["disk"]["files_end_bytes"] = {k: v[1] for k, v in sorted(g.items())}
out["disk"]["files_growth_bytes"] = {k: v[1] - v[0] for k, v in sorted(g.items())}
# Durable growth: database main files + log buffer files (WAL/SHM are reused, bounded scratch).
dur = sum(v[1] - v[0] for k, v in g.items() if k in ("minicron.db", "minicron-logs.db", "log-buffers/other"))
out["disk"]["durable_growth_kib_per_min"] = round(dur / 1024 / (win / 60), 1)
out["disk"]["metadata_db_growth_kib_per_min"] = round((g.get("minicron.db", [0, 0])[1] - g.get("minicron.db", [0, 0])[0]) / 1024 / (win / 60), 1)
out["disk"]["archive_db_growth_kib_per_min"] = round((g.get("minicron-logs.db", [0, 0])[1] - g.get("minicron-logs.db", [0, 0])[0]) / 1024 / (win / 60), 1)
out["disk"]["log_buffer_growth_kib_per_min"] = round((g.get("log-buffers/other", [0, 0])[1] - g.get("log-buffers/other", [0, 0])[0]) / 1024 / (win / 60), 1)

for which in ("start", "end"):
    try:
        mj = json.load(open(os.path.join(d, f"monitor-{which}.json")))
        out[f"goroutines_{which}"] = mj.get("goroutines")
        out[f"heap_objects_mib_{which}"] = round(mj.get("heap_bytes", 0) / 2**20, 2)
    except Exception:
        pass

st = os.path.join(d, "strace.txt")
if os.path.exists(st) and float(meta.get("strace_s", 0)) > 0:
    s = float(meta["strace_s"]); calls = {}
    for line in open(st):
        m = re.match(r"\s*[\d.]+\s+[\d.]+\s+\d+\s+(\d+)\s+(?:\d+\s+)?(\w+)\s*$", line)
        if m:
            calls[m.group(2)] = int(m.group(1))
        m = re.match(r"\s*100\.00\s+[\d.]+\s+\d+\s+(\d+)\s+(?:\d+\s+)?total", line)
        if m:
            calls["total"] = int(m.group(1))
    keys = ["fsync", "fdatasync", "write", "pwrite64", "read", "pread64", "futex", "epoll_pwait",
            "nanosleep", "openat", "close", "wait4", "waitid", "clone", "clone3", "total"]
    out["strace_per_s"] = {k: round(calls[k] / s, 2) for k in keys if k in calls}
    out["strace_threads"] = [open(os.path.join(d, f"strace-threads-{x}")).read().strip() for x in ("before", "after")]

out["shutdown_s"] = float(meta["shutdown_s"])
out["ready_s"] = round(float(meta["t_ready"]) - float(meta["t_start"]), 3)

# Run table (read after shutdown from a copy; nothing queried it during the run).
tmp = tempfile.mkdtemp()
try:
    for f in os.listdir(os.path.join(d, "data")):
        if f.startswith("minicron.db"):
            shutil.copy(os.path.join(d, "data", f), tmp)
    db = sqlite3.connect(os.path.join(tmp, "minicron.db"))
    w0 = int(float(meta["window_start"]) * 1e6); w1 = int(float(meta["window_end"]) * 1e6)
    rs = db.execute("select run_id, job, kind, status, trigger, scheduled_for_us, queued_us, started_us, ended_us, log_bytes, end_reason "
                    "from runs where queued_us >= ? and queued_us < ? order by queued_us", (w0, w1)).fetchall()
    by = {}
    for r in rs:
        by.setdefault(r[1], {}).setdefault(r[3], 0)
        by[r[1]][r[3]] += 1
    out["runs_in_window"] = len(rs)
    out["runs_by_job_status"] = by
    sched = [r for r in rs if r[4] == "schedule" and r[5] and r[7]]
    out["fire_queue_lag_ms"] = dist([(r[6] - r[5]) / 1000 for r in sched])
    out["fire_start_lag_ms"] = dist([(r[7] - r[5]) / 1000 for r in sched])
    for jobset, name in ((("tick-echo", "tick-true"), "fire_start_lag_1s_jobs_ms"),
                         (("job-lines", "job-burst", "job-silent", "job-mixed"), "fire_start_lag_5s_jobs_ms")):
        out[name] = dist([(r[7] - r[5]) / 1000 for r in sched if r[1] in jobset])
    # child-side start timestamps
    evs = {}
    ep = os.path.join(d, "ev", "jobs.events")
    if os.path.exists(ep):
        for l in open(ep):
            t, kind, rid, *_ = l.split()
            evs[rid] = float(t)
    srt = {r[0]: r for r in sched}
    out["fire_child_lag_ms"] = dist([(evs[k] * 1e6 - srt[k][5]) / 1000 for k in evs if k in srt])
    # job duration/completion: ended - started for job runs that succeeded
    # crash-loop worker restart latency
    cr = [r for r in db.execute("select run_id, started_us, ended_us from runs where job='crash-1' and started_us is not null "
                                "and queued_us >= ? and queued_us < ? order by queued_us", (w0, w1))]
    out["restart_gap_daemon_ms"] = dist([(n[1] - p[2]) / 1000 - 2000 for p, n in zip(cr, cr[1:]) if p[2] and n[1]])
    ce = os.path.join(d, "ev", "crash.events")
    if os.path.exists(ce):
        seq = []
        for l in open(ce):
            t, kind, rid = l.split()
            seq.append((float(t), kind, rid))
        starts = {rid: t for t, k, rid in seq if k == "start"}
        exits = {rid: t for t, k, rid in seq if k == "exit"}
        ids = [r[0] for r in cr]
        out["restart_gap_child_ms"] = dist([(starts[n] - exits[p]) * 1000 - 2000 for p, n in zip(ids, ids[1:]) if p in exits and n in starts])
        out["exit_to_ended_ms"] = dist([(r[2] / 1e6 - exits[r[0]]) * 1000 for r in cr if r[0] in exits and r[2]])
    db.close()
finally:
    shutil.rmtree(tmp)

log = open(os.path.join(d, "daemon.log")).read().splitlines()
out["daemon_log_lines"] = len(log)
out["daemon_log_warn_error"] = sum(1 for l in log if "level=WARN" in l or "level=ERROR" in l)
print(json.dumps(out, indent=1))
