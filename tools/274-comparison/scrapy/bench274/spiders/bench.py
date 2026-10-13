"""#274 phase-3 spider: fetch the 1000 fixture homepages.

URLs come from /tmp/274-urls.txt (one per line). Robots handling is done by
Scrapy's built-in middleware (ROBOTSTXT_OBEY=True): per domain the robots.txt
fetch goes through the downloader first, then the page — with DOWNLOAD_DELAY=1
and CONCURRENT_REQUESTS_PER_DOMAIN=1 the page lands >=1s after robots, which
is the binding constraint measured for crawler-node's faithful run.
"""
import json
import time

import scrapy
from scrapy import signals


class BenchSpider(scrapy.Spider):
    name = "bench274"

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.bytes_total = 0

    @classmethod
    def from_crawler(cls, crawler, *args, **kwargs):
        spider = super().from_crawler(crawler, *args, **kwargs)
        crawler.signals.connect(spider._closed, signal=signals.spider_closed)
        return spider

    def _closed(self, spider):
        mw = self.crawler.engine.downloader.middleware.middlewares
        for m in mw:
            if type(m).__name__ == "GapMiddleware" and hasattr(m, "spider_closed"):
                m.spider_closed(spider)
        # Machine-readable result summary for the #274 comparison table.
        stats = self.crawler.stats.get_stats()
        with open("/tmp/274-scrapy-stats.json", "w") as f:
            json.dump({
                "pages_ok": stats.get("bench274/pages_ok", 0),
                "pages_err": stats.get("bench274/pages_err", 0),
                "bytes": self.bytes_total,
                "elapsed_s": stats.get("elapsed_time_seconds", 0),
                "finished_at": time.time(),
            }, f)

    async def start(self):
        # Scrapy >= 2.13 entry point (start_requests is no longer consulted).
        with open("/tmp/274-urls.txt") as f:
            urls = [line.strip() for line in f if line.strip()]
        for url in urls:
            yield scrapy.Request(url, callback=self.parse, errback=self.err,
                                 meta={"download_timeout": 30})

    def start_requests(self):
        # Legacy entry point, kept for Scrapy < 2.13.
        return self.start()

    def parse(self, response):
        assert response.status == 200, response.url
        assert b"<title>" in response.body, response.url
        self.crawler.stats.inc_value("bench274/pages_ok")
        self.bytes_total += len(response.body)

    def err(self, failure):
        self.crawler.stats.inc_value("bench274/pages_err")
        self.logger.warning("fetch failed: %r", failure.value)
