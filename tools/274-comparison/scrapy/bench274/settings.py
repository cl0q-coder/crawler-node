# #274 phase-3 Scrapy runner. See ../COMPARISON.md (repo root) for methodology.
# Settings mirror the production-faithful crawler-node run:
# 16 concurrent requests, 1s politeness delay per domain,
# robots.txt obeyed (page fetch follows robots fetch per domain).
BOT_NAME = "bench274"
SPIDER_MODULES = ["bench274.spiders"]
NEWSPIDER_MODULE = "bench274.spiders"

CONCURRENT_REQUESTS = 16
CONCURRENT_REQUESTS_PER_DOMAIN = 1
DOWNLOAD_DELAY = 1
RANDOMIZE_DOWNLOAD_DELAY = False

ROBOTSTXT_OBEY = True

# Fixture uses a throwaway self-signed CA; skip verification (test-only),
# exactly like the baseline harness's InsecureSkipVerify. The TLS handshake
# (crypto) is still fully exercised.
DOWNLOADER_CLIENTCONTEXTFACTORY = "bench274.tls.InsecureContextFactory"

DOWNLOADER_MIDDLEWARES = {
    "bench274.gapmw.GapMiddleware": 100,
}

# Keep it lean: no cookies, no retries (fixture never fails), minimal logging.
COOKIES_ENABLED = False
RETRY_ENABLED = False
LOG_LEVEL = "WARNING"
TELNETCONSOLE_ENABLED = False
