#!/usr/bin/env python3
"""Sample one process (daemon only; children are separate PIDs) from /proc.

Usage: sampler.py <pid> <seconds> <interval_s> <out.csv> [data_dir]

Columns are cumulative counters at each tick; analysis takes deltas.
- utime/stime: /proc/pid/stat clock ticks (includes exited threads)
- run_ns/wait_ns/slices: sum of /proc/pid/task/*/schedstat (live threads)
- vcsw/ivcsw: sum of per-thread voluntary/nonvoluntary context switches
- rss_kb/hwm_kb/threads: /proc/pid/status
- io: /proc/pid/io (rchar wchar syscr syscw read_bytes write_bytes)
- data_bytes: apparent size of data_dir (sum of st_size) when given
"""
import os, sys, time

def read(path):
    with open(path) as f:
        return f.read()

def stat_ticks(pid):
    s = read(f"/proc/{pid}/stat")
    rest = s[s.rindex(")") + 2:].split()
    # fields after comm: state(0) ... utime is field 14 -> index 11, stime 12
    return int(rest[11]), int(rest[12])

def status(pid):
    out = {}
    for line in read(f"/proc/{pid}/status").splitlines():
        k, _, v = line.partition(":")
        out[k] = v.strip()
    return out

def tasks(pid):
    run = wait = slices = vcs = ivcs = 0
    for t in os.listdir(f"/proc/{pid}/task"):
        try:
            a, b, c = read(f"/proc/{pid}/task/{t}/schedstat").split()
            run += int(a); wait += int(b); slices += int(c)
            for line in read(f"/proc/{pid}/task/{t}/status").splitlines():
                if line.startswith("voluntary_ctxt_switches"):
                    vcs += int(line.split()[1])
                elif line.startswith("nonvoluntary_ctxt_switches"):
                    ivcs += int(line.split()[1])
        except (FileNotFoundError, ProcessLookupError, ValueError):
            pass
    return run, wait, slices, vcs, ivcs

def io(pid):
    d = {}
    for line in read(f"/proc/{pid}/io").splitlines():
        k, _, v = line.partition(":")
        d[k] = int(v)
    return d

def host():
    load1 = read("/proc/loadavg").split()[0]
    cpu = [int(x) for x in read("/proc/stat").splitlines()[0].split()[1:]]
    # user nice system idle iowait irq softirq steal
    total = sum(cpu[:8]); idle = cpu[3] + cpu[4]
    return [load1, total - idle, total, cpu[7], cpu[4]]

def blk():
    """Block-device flush requests and write IOs (system-wide; a proxy for
    fsync rate on an otherwise quiet host). Device from BLKDEV, default sda."""
    try:
        f = read(f"/sys/block/{os.environ.get('BLKDEV', 'sda')}/stat").split()
        return [f[15], f[4]]  # flush requests completed, write IOs completed
    except (FileNotFoundError, IndexError):
        return ["", ""]

def dsize(root):
    total = 0
    for dp, dn, fn in os.walk(root):
        for f in fn:
            try:
                total += os.lstat(os.path.join(dp, f)).st_size
            except FileNotFoundError:
                pass
    return total

def main():
    pid = int(sys.argv[1]); dur = float(sys.argv[2]); iv = float(sys.argv[3])
    out = sys.argv[4]; data = sys.argv[5] if len(sys.argv) > 5 else None
    cols = ["t", "utime", "stime", "run_ns", "wait_ns", "slices", "vcsw", "ivcsw",
            "rss_kb", "hwm_kb", "threads", "rchar", "wchar", "syscr", "syscw",
            "read_bytes", "write_bytes", "data_bytes", "load1", "host_busy", "host_total", "host_steal", "host_iowait", "blk_flush", "blk_wr_ios"]
    t0 = time.monotonic()
    with open(out, "w") as f:
        f.write(",".join(cols) + "\n")
        n = 0
        while True:
            now = time.monotonic() - t0
            try:
                ut, st = stat_ticks(pid)
                s = status(pid)
                run, wait, sl, vcs, ivcs = tasks(pid)
                i = io(pid)
            except (FileNotFoundError, ProcessLookupError):
                break
            ds = dsize(data) if data and (n % 10 == 0 or now + iv > dur) else ""
            row = [f"{now:.3f}", ut, st, run, wait, sl, vcs, ivcs,
                   s["VmRSS"].split()[0], s["VmHWM"].split()[0], s["Threads"],
                   i["rchar"], i["wchar"], i["syscr"], i["syscw"],
                   i["read_bytes"], i["write_bytes"], ds] + host() + blk()
            f.write(",".join(map(str, row)) + "\n"); f.flush()
            n += 1
            if now >= dur:
                break
            time.sleep(max(0.0, t0 + n * iv - time.monotonic()))

if __name__ == "__main__":
    main()
