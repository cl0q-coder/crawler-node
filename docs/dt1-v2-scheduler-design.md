# DT-1: v2.0 Scheduler — Production Reference Implementation

## Overview

The v2.0 scheduler combines two techniques for crawl prioritization:

1. **OPIC T2** (streaming PageRank approximation) — orders the frontier by page importance
2. **SimHash IG2** (near-duplicate detection) — skips re-crawling near-duplicate content

Both are already implemented in `cmd/website-crawler/`. This document describes
the production reference implementation: the schema, the migration, and the
operational characteristics.

## OPIC T2

**Paper:** Abiteboul et al., "Adaptive On-Line Page Importance Computation", WWW 2003.

**State per domain (O(1)):**
- `domain_info.opic_cash` (REAL, default 1.0) — unspent importance
- `domain_info.opic_history` (REAL, default 0) — accumulated importance

**Algorithm:**
1. When a page is crawled, its `opic_cash` is distributed equally among its outlink domains
2. The crawled domain's cash resets to 0, and the spent amount moves to `opic_history`
3. Frontier score = `opic_history + opic_cash`, ordered DESC

**Self-link handling:** Self-links are filtered BEFORE dividing (see #110 fix).
Otherwise they dilute the per-destination share.

**New domains:** Start with cash=1.0 (OPIC initial value).

**Frontier query:** Uses partial index `idx_domain_info_opic_score` on
`((opic_history + opic_cash) DESC) WHERE web_crawled_at IS NULL AND dns_fail_count < 3`.

## SimHash IG2

**Fingerprint:** 64-bit SimHash of page content (`website_info.simhash`).

**Block indexing:** The 64-bit hash is split into 4×16-bit blocks
(`simhash_b0`..`simhash_b3`). A near-duplicate must match at least one block
exactly (pigeonhole principle). Each block has its own B-tree index.

**Duplicate check (batched):**
1. Collect all SimHashes from the flush batch
2. Single query: `WHERE simhash_b0 = ANY($1) OR ...` (see #110 fix — was N+1)
3. Hamming distance check in Go via `simhash.IsDuplicate()`
4. If duplicate found, set `website_info.duplicate_of` to the canonical domain

**Zero hash:** `simhash = 0` means "not computed" — skipped in duplicate checks.

## Migration

**File:** `migrations/001_dt1_v2_scheduler.sql`

**Idempotent:** All statements use `IF NOT EXISTS`. Safe to re-run.

**CONCURRENTLY:** Index builds use `CREATE INDEX CONCURRENTLY` to avoid
locking the 13M+ row tables. Cannot run inside a transaction — execute
statements individually or use a CONCURRENTLY-aware runner.

**Columns:**
| Table | Column | Type | Default | Purpose |
|-------|--------|------|---------|---------|
| domain_info | opic_cash | REAL | 1.0 | Unspent OPIC importance |
| domain_info | opic_history | REAL | 0 | Accumulated OPIC importance |
| website_info | simhash | BIGINT | NULL | 64-bit content fingerprint |
| website_info | simhash_b0..b3 | SMALLINT | NULL | Fingerprint blocks for indexing |
| website_info | duplicate_of | TEXT | NULL | Canonical domain if near-duplicate |

## Operational Notes

- **Backfill:** Existing rows get defaults (cash=1.0, history=0, simhash=NULL).
  SimHashes populate on next crawl. No backfill required for correctness —
  OPIC converges from the initial uniform distribution.
- **Index build time:** CONCURRENTLY on 13M rows takes ~10-30 minutes per index.
  Build during low-traffic window.
- **Monitoring:** `crawler_frontier_depth` gauge shows uncrawled count.
  OPIC scores are visible via `(opic_history + opic_cash)` in queries.
