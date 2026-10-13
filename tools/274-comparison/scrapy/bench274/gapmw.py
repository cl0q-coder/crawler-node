"""Downloader middleware recording per-domain dispatch timestamps so the
minimum observed per-host gap (politeness compliance) can be computed.

The spider calls spider_closed() on shutdown (see spiders/bench.py), which
writes /tmp/274-scrapy-gaps.json.
"""
import json
import time
from urllib.parse import urlparse


class GapMiddleware:
    def __init__(self):
        self.last = {}
        self.gaps = []

    def process_request(self, request, spider):
        host = urlparse(request.url).hostname
        now = time.monotonic()
        prev = self.last.get(host)
        if prev is not None:
            self.gaps.append(now - prev)
        self.last[host] = now
        return None

    def spider_closed(self, spider):
        out = {
            "n_gaps": len(self.gaps),
            "min_gap_s": min(self.gaps) if self.gaps else None,
        }
        with open("/tmp/274-scrapy-gaps.json", "w") as f:
            json.dump(out, f)
