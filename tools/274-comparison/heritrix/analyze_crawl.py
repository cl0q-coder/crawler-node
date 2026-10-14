#!/usr/bin/env python3
"""Analyze a Heritrix crawl.log for #274 phase 3.
Reports: total fetches, 200 OK seed pages, robots fetches, failures,
and per-host robots->page gap stats (politeness verification).
Usage: analyze_crawl.py <crawl.log>
"""
import re
import sys
from datetime import datetime

log = sys.argv[1]
# crawl.log: "2026-10-13T06:15:22.123Z   200         532 https://bench12.test:40659/robots.txt ..."
pat = re.compile(r"^(\S+)\s+(-?\d+)\s+\d+\s+(\S+)")
hosts = {}
pages_ok = 0
robots_ok = 0
fails = []
total = 0
for line in open(log, errors="replace"):
    m = pat.match(line)
    if not m:
        continue
    ts, code, url = m.group(1), int(m.group(2)), m.group(3)
    total += 1
    hm = re.match(r"https://(bench\d+\.test):\d+(\/\S*)", url)
    if not hm:
        continue
    host, path = hm.group(1), hm.group(2)
    try:
        t = datetime.fromisoformat(ts.replace("Z", "+00:00")).timestamp()
    except ValueError:
        continue
    d = hosts.setdefault(host, {})
    if path == "/robots.txt":
        d["robots"] = t
        if code == 200:
            robots_ok += 1
        else:
            fails.append((host, "robots", code))
    elif path == "/":
        d["page"] = t
        if code == 200:
            pages_ok += 1
        else:
            fails.append((host, "page", code))

gaps = []
for host, d in hosts.items():
    if "robots" in d and "page" in d:
        gaps.append(d["page"] - d["robots"])
gaps.sort()
print(f"log_lines={total} hosts_seen={len(hosts)}")
print(f"seed_pages_200={pages_ok} robots_200={robots_ok} failures={len(fails)}")
for h, w, c in fails[:10]:
    print(f"  FAIL {h} {w} code={c}")
if gaps:
    n = len(gaps)
    print(f"per_host_robots_to_page_gap_s: n={n} min={gaps[0]:.3f} "
          f"p50={gaps[n//2]:.3f} p90={gaps[int(n*0.9)]:.3f} max={gaps[-1]:.3f}")
    below = sum(1 for g in gaps if g < 0.999)
    print(f"gaps_below_1s_floor={below}")
