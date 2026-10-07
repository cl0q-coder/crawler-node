-- DT-1: v2.0 scheduler production reference implementation
-- OPIC T2 (domain_info.opic_cash, domain_info.opic_history)
-- SimHash IG2 (website_info.simhash, website_info.simhash_b0..b3, website_info.duplicate_of)
--
-- IDEMPOTENT: safe to run multiple times. Uses IF NOT EXISTS throughout.
-- CONCURRENTLY: index builds do not block reads/writes on the 13M+ row tables.
--   Note: CREATE INDEX CONCURRENTLY cannot run inside a transaction block.
--   Run each statement separately, or use a migration runner that handles this.

-- OPIC T2: cash and history columns on domain_info
ALTER TABLE domain_info ADD COLUMN IF NOT EXISTS opic_cash REAL NOT NULL DEFAULT 1.0;
ALTER TABLE domain_info ADD COLUMN IF NOT EXISTS opic_history REAL NOT NULL DEFAULT 0;

-- SimHash IG2: fingerprint and block columns on website_info
ALTER TABLE website_info ADD COLUMN IF NOT EXISTS simhash BIGINT;
ALTER TABLE website_info ADD COLUMN IF NOT EXISTS simhash_b0 SMALLINT;
ALTER TABLE website_info ADD COLUMN IF NOT EXISTS simhash_b1 SMALLINT;
ALTER TABLE website_info ADD COLUMN IF NOT EXISTS simhash_b2 SMALLINT;
ALTER TABLE website_info ADD COLUMN IF NOT EXISTS simhash_b3 SMALLINT;
ALTER TABLE website_info ADD COLUMN IF NOT EXISTS duplicate_of TEXT;

-- OPIC score index: orders the frontier by (history + cash) DESC.
-- Partial index: only uncrawled, DNS-healthy domains are in the frontier.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_domain_info_opic_score
    ON domain_info ((opic_history + opic_cash) DESC)
    WHERE web_crawled_at IS NULL AND dns_fail_count < 3;

-- SimHash block indexes: findDuplicate queries WHERE simhash_b0 = $1 OR ...
-- Each block gets its own index for the OR query planner.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_website_info_simhash_b0 ON website_info (simhash_b0);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_website_info_simhash_b1 ON website_info (simhash_b1);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_website_info_simhash_b2 ON website_info (simhash_b2);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_website_info_simhash_b3 ON website_info (simhash_b3);
