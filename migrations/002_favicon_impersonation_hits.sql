-- 002_favicon_impersonation_hits.sql
-- #111 v1: node-local table for favicon brand-impersonation hits.
-- Written by the favicon-scan batch tool (ensureSchema is idempotent and
-- covers existing installs); this migration covers fresh initdb installs.
CREATE TABLE IF NOT EXISTS favicon_impersonation_hits (
    domain       text NOT NULL,
    brand_name   text NOT NULL,
    sha256       text NOT NULL,
    ahash        text,
    hamming_dist smallint,
    is_exact     boolean NOT NULL DEFAULT true,
    detected_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (domain, brand_name)
);
CREATE INDEX IF NOT EXISTS idx_favicon_hits_brand ON favicon_impersonation_hits (brand_name);
CREATE INDEX IF NOT EXISTS idx_favicon_hits_detected ON favicon_impersonation_hits (detected_at);
