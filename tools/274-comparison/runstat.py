#!/usr/bin/env python3
"""runstat.py — run a command, report wall time, peak RSS (process tree),
and total CPU seconds (process tree, cumulative per-PID).

Usage: runstat.py <label> <command...>
Samples /proc every 0.2s. CPU counters (utime+stime) are cumulative per PID,
so per-PID maxima summed at the end give the tree's total CPU time even for
short-lived children.
"""
import os
import subprocess
import sys
import time

label = sys.argv[1]
cmd = sys.argv[2:]
HZ = os.sysconf(os.sysconf_names["SC_CLK_TCK"])


def children(root):
    """All PIDs in root's process tree (BFS over /proc ppid)."""
    ppid_of = {}
    for pid in filter(str.isdigit, os.listdir("/proc")):
        try:
            with open(f"/proc/{pid}/stat") as f:
                data = f.read()
            rparen = data.rfind(")")
            ppid = int(data[rparen + 2 :].split()[1])
            ppid_of[int(pid)] = ppid
        except Exception:
            continue
    tree = {root}
    frontier = [root]
    while frontier:
        nxt = []
        for pid, ppid in ppid_of.items():
            if ppid in tree and pid not in tree:
                tree.add(pid)
                nxt.append(pid)
        frontier = nxt
    return tree


def sample(pid):
    """(rss_kb, cpu_ticks) or None."""
    try:
        with open(f"/proc/{pid}/status") as f:
            rss = 0
            for line in f:
                if line.startswith("VmHWM:"):
                    rss = int(line.split()[1])
                    break
        with open(f"/proc/{pid}/stat") as f:
            data = f.read()
        parts = data[data.rfind(")") + 2 :].split()
        cpu = int(parts[11]) + int(parts[12])  # utime + stime
        return rss, cpu
    except Exception:
        return None


start = time.monotonic()
proc = subprocess.Popen(cmd)
peak_rss = 0
cpu_max = {}
while proc.poll() is None:
    for pid in children(proc.pid):
        s = sample(pid)
        if s:
            rss, cpu = s
            peak_rss = max(peak_rss, rss)
            cpu_max[pid] = max(cpu_max.get(pid, 0), cpu)
    time.sleep(0.2)
# one final sweep for children that exited between last sample and wait
for pid in children(proc.pid):
    s = sample(pid)
    if s:
        cpu_max[pid] = max(cpu_max.get(pid, 0), s[1])
rc = proc.wait()
wall = time.monotonic() - start
cpu_s = sum(cpu_max.values()) / HZ
print(f"RUNSTAT label={label} rc={rc} wall_s={wall:.2f} "
      f"peak_rss_tree_mb={peak_rss / 1024:.1f} cpu_tree_s={cpu_s:.1f} "
      f"pids_tracked={len(cpu_max)}")
