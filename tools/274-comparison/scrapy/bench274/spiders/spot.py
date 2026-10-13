"""#274 phase-3 Scrapy politeness spot check: ONE host, robots.txt then page,
measures the client-observed gap between the two response completions.

Same politeness settings as the full run (DOWNLOAD_DELAY=1,
CONCURRENT_REQUESTS_PER_DOMAIN=1). Prints SPOT_GAP=<seconds>.
"""
import time

import scrapy


class SpotSpider(scrapy.Spider):
    name = "spot274"

    custom_settings = {
        "CONCURRENT_REQUESTS": 16,
        "CONCURRENT_REQUESTS_PER_DOMAIN": 1,
        "DOWNLOAD_DELAY": 1,
        "ROBOTSTXT_OBEY": False,  # we fetch robots.txt explicitly
        "COOKIES_ENABLED": False,
        "RETRY_ENABLED": False,
        "LOG_LEVEL": "WARNING",
        "TELNETCONSOLE_ENABLED": False,
        "DOWNLOADER_CLIENTCONTEXTFACTORY": "bench274.tls.InsecureContextFactory",
    }

    async def start(self):
        import os

        port = open("/tmp/fixture-port.txt").read().strip().split("=")[1]
        self.base = f"https://bench0.test:{port}"
        yield scrapy.Request(
            self.base + "/robots.txt",
            callback=self.after_robots,
            meta={"dont_obey_robotstxt": True, "download_timeout": 30},
        )

    def after_robots(self, response):
        assert response.status == 200
        self.t_robots_done = time.monotonic()
        yield scrapy.Request(
            self.base + "/",
            callback=self.after_page,
            meta={"dont_obey_robotstxt": True, "download_timeout": 30},
        )

    def after_page(self, response):
        assert response.status == 200
        gap = time.monotonic() - self.t_robots_done
        print(f"SPOT_GAP robots_done_to_page_done_s={gap:.3f}")
