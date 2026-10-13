#!/usr/bin/env python3
"""Per-host robots.txt -> page gap analysis for the #274 Nutch leg.

Reads the fixturelog request log ("<unixnano> <host:port> <path>") and
reports, per host, the gap between the robots.txt GET and the page GET.
Usage: python3 analyze_requests.py [requests.log]
"""
import statistics
import sys
from collections import defaultdict

path = sys.argv[1] if len(sys.argv) > 1 else "requests.log"
hits = defaultdict(dict)  # host -> {path: earliest_ns}
n_lines = 0
for line in open(path):
    parts = line.split()
    if len(parts) != 3:
        continue
    ns, hostport, p = parts
    host = hostport.rsplit(":", 1)[0]
    ns = int(ns)
    n_lines += 1
    if p not in hits[host] or ns < hits[host][p]:
        hits[host][p] = ns

robots = sum(1 for h in hits.values() if "/robots.txt" in h)
pages = sum(1 for h in hits.values() if "/" in h)
print(f"lines={n_lines} hosts={len(hits)} hosts_with_robots={robots} hosts_with_page={pages}")

gaps = []
missing = 0
for host, d in hits.items():
    if "/robots.txt" in d and "/" in d:
        gaps.append((d["/"] - d["/robots.txt"]) / 1e9)
    else:
        missing += 1
gaps.sort()
print(f"hosts_with_both={len(gaps)} missing_one={missing}")
if gaps:
    print(f"robots->page gap (s): min={gaps[0]:.3f} p50={statistics.median(gaps):.3f} "
          f"p95={gaps[int(0.95*len(gaps))]:.3f} max={gaps[-1]:.3f} "
          f"mean={statistics.mean(gaps):.3f}")
    below = sum(1 for g in gaps if g < 1.0)
    print(f"below_1s_floor={below}/{len(gaps)}")
    # histogram buckets
    for lo, hi in [(0, 0.01), (0.01, 0.1), (0.1, 0.5), (0.5, 1.0), (1.0, 2.0), (2.0, 1e9)]:
        c = sum(1 for g in gaps if lo <= g < hi)
        print(f"  [{lo:>4},{hi:>4}): {c}")
