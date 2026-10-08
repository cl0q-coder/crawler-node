-- HE-4: freshness + recrawl scheduling (crawler-node #146)
--
-- Per-domain change-rate EWMA, materialized recrawl priority score, and the
-- sitemap-lastmod staleness hint. All columns are NULL-able and backfilled
-- lazily by the HE-4 score sweeper — no downtime, no backfill scan.
--
-- IDEMPOTENT: safe to run multiple times. Uses IF NOT EXISTS throughout.
-- CONCURRENTLY: the index build does not block reads/writes on the 13M+ row
--   tables. Note: CREATE INDEX CONCURRENTLY cannot run inside a transaction
--   block. Run each statement separately, or use a migration runner that
--   handles this.
--
-- The website-crawler also ensures these columns at startup
-- (ensureHe4Columns) so nodes that never apply migrations stay working.

-- change_rate: estimated content changes/day (EWMA, alpha=0.3), clamped to
-- [1/365, 2.0]. NULL = unseen → the sweeper treats it as 1/120.
ALTER TABLE domain_info ADD COLUMN IF NOT EXISTS change_rate REAL;

-- next_score: materialized recrawl priority P = importance x expected_gain x
-- overdue (x sitemap boost). Written by the background sweeper for due rows
-- only; NULL = not due / not yet scored. The hot draw reads it via the
-- partial index below as an index range scan.
ALTER TABLE domain_info ADD COLUMN IF NOT EXISTS next_score REAL;

-- sitemap_lastmod: latest <lastmod> seen in the domain's sitemap (from the
-- T1 sitemap fetch pass). A lastmod newer than web_crawled_at is a pre-fetch
-- staleness hint (design §3): the sweeper promotes the domain before we
-- spend a fetch. Hint only — the fetch-time SimHash delta stays ground truth.
ALTER TABLE domain_info ADD COLUMN IF NOT EXISTS sitemap_lastmod TIMESTAMPTZ;

-- Hot draw: SELECT ... FROM domain_info WHERE next_score IS NOT NULL
-- ORDER BY next_score DESC LIMIT $1 — a pure index range scan. Partial: only
-- scored (due) rows are in the index.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_domain_info_next_score
    ON domain_info (next_score DESC)
    WHERE next_score IS NOT NULL;
