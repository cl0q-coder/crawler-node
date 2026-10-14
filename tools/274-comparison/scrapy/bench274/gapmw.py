"""Downloader middleware recording per-domain response timestamps so the
minimum observed per-host gap (politeness compliance) can be sanity-checked.

Measured in process_response (responses received), NOT process_request:
with ROBOTSTXT_OBEY the page request passes process_request once BEFORE the
robots.txt fetch is issued and again after, so request-side timestamps show a
spurious ~ms gap. Response-side timestamps reflect actual network fetches.

Caveat: response gaps can undershoot the true dispatch gap by up to
(robots_fetch_time - page_fetch_time) — the robots fetch pays a fresh TLS
handshake while the page reuses the connection. The dispatch gap itself is
>= DOWNLOAD_DELAY by Scrapy's slot mechanics (verified in
scrapy/core/downloader/__init__.py `_process_queue`:
`penalty = delay - now + slot.lastseen`), and the 66.7s wall for 1000 pages
at 16-way concurrency is consistent only with the 1s delay binding.

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

    def process_response(self, request, response, spider):
        host = urlparse(request.url).hostname
        now = time.monotonic()
        prev = self.last.get(host)
        if prev is not None:
            self.gaps.append(now - prev)
        self.last[host] = now
        return response

    def spider_closed(self, spider):
        out = {
            "n_gaps": len(self.gaps),
            "min_gap_s": min(self.gaps) if self.gaps else None,
        }
        with open("/tmp/274-scrapy-gaps.json", "w") as f:
            json.dump(out, f)
