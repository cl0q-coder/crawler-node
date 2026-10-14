package main

import (
	"database/sql"
	"fmt"
)

// ensureSchema creates the node-local hits table (idempotent; also covered by
// migrations/002_favicon_impersonation_hits.sql for fresh initdb installs).
func ensureSchema(db *sql.DB) error {
	_, err := db.Exec(`
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
`)
	return err
}

// loadDomains reads up to limit crawled domains (most recently crawled first).
func loadDomains(db *sql.DB, limit int) ([]string, error) {
	rows, err := db.Query(`
SELECT domain FROM domain_info
WHERE web_crawled_at IS NOT NULL AND domain IS NOT NULL AND domain != ''
ORDER BY web_crawled_at DESC
LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// saveHits upserts exact-match hits (non-excluded) into the hits table.
func saveHits(db *sql.DB, hits []Hit) (int64, error) {
	var n int64
	for _, h := range hits {
		res, err := db.Exec(`
INSERT INTO favicon_impersonation_hits (domain, brand_name, sha256, ahash, hamming_dist, is_exact)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (domain, brand_name) DO UPDATE SET
    sha256 = EXCLUDED.sha256,
    ahash = EXCLUDED.ahash,
    hamming_dist = EXCLUDED.hamming_dist,
    is_exact = EXCLUDED.is_exact,
    detected_at = now()`,
			h.Domain, h.Brand, h.SHA256, h.AHashHex, h.HammingDist, h.IsExact)
		if err != nil {
			return n, fmt.Errorf("upsert %s: %w", h.Domain, err)
		}
		if c, err := res.RowsAffected(); err == nil {
			n += c
		}
	}
	return n, nil
}
