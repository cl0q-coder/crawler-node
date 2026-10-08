package main

// HE-4: freshness + recrawl scheduling (crawler-node #146).
//
// This file holds the HE-4 core: change-detection verdicts (SimHash delta),
// the change-rate EWMA, the staleness priority score, the λ-aware T3 interval
// factors, the background score sweeper, and the per-batch signal updates.
//
// Design: docs/he4-freshness-recrawl.md. §§1–6 (single-node) are implemented
// here; §7 (fleet) is provisional spec only — no client code until HE-1 #135
// agrees the PROTOCOL v2 wire.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"cl0q/internal/simhash"
	"github.com/lib/pq"
)

const (
	// he4SimHashChangeBits: Hamming distance >= 8 bits on the 64-bit SimHash
	// counts as "content changed" (design §3). Starting guess — calibrate
	// against the labeled IG2 near-dup set before locking in (design Q3).
	he4SimHashChangeBits = 8

	// he4EWMAAlpha: weight of each new observation in the change-rate EWMA.
	he4EWMAAlpha = 0.3
	// he4ChangeRateDefault: λ for unseen domains (1 change / 120 days).
	he4ChangeRateDefault = 1.0 / 120
	// he4ChangeRateMin/Max: EWMA clamp — [1/365, 2.0] changes/day.
	he4ChangeRateMin = 1.0 / 365
	he4ChangeRateMax = 2.0

	// he4SitemapBoost: multiplicative P boost when sitemap_lastmod is newer
	// than our web_crawled_at (pre-fetch staleness hint, design §3). Hint
	// only — the fetch-time SimHash delta stays ground truth (design Q4).
	he4SitemapBoost = 2.0
)

// he4ChangeObserved classifies one recrawl outcome into a change verdict.
//
// notModified: the server answered 304 to our conditional GET → the stored
// copy is definitively current (cheapest signal, design §3.1).
//
// Otherwise the verdict comes from the SimHash delta: Hamming distance >=
// he4SimHashChangeBits between the fresh fingerprint and the stored one =
// content changed (design §3.2). If either fingerprint is missing (0 = not
// computed), the verdict is unknown — the caller falls back to legacy
// behavior (treat a 200 as changed for T3, skip the EWMA update).
func he4ChangeObserved(notModified bool, freshFP, storedFP uint64) (changed, known bool) {
	if notModified {
		return false, true
	}
	if freshFP == 0 || storedFP == 0 {
		return false, false
	}
	return simhash.Distance(freshFP, storedFP) >= he4SimHashChangeBits, true
}

// he4NewChangeRate is the EWMA update for domain_info.change_rate:
// λ ← (1-α)·λ + α·obs, obs = 1 on observed change, 0 on observed no-change,
// clamped to [1/365, 2.0]. An old <= 0 (NULL/unset) starts at the 1/120 default.
// Go reference for the EWMA UPDATE in he4UpdateSignals (parity-tested).
func he4NewChangeRate(old float64, changed bool) float64 {
	if old <= 0 {
		old = he4ChangeRateDefault
	}
	obs := 0.0
	if changed {
		obs = 1.0
	}
	v := old*(1-he4EWMAAlpha) + he4EWMAAlpha*obs
	if v < he4ChangeRateMin {
		return he4ChangeRateMin
	}
	if v > he4ChangeRateMax {
		return he4ChangeRateMax
	}
	return v
}

// he4PriorityScore is the HE-4 recrawl priority (design §4):
// P = importance × expected_gain × overdue (× sitemap boost).
//
// importance  = opic_history + opic_cash (link-earned importance)
// expected_gain = 1 - exp(-λ·age_days)  (Poisson P(changed since our copy))
// overdue     = age_days / interval_days (starvation guard: low-λ pages still
//               get crawled, on their long interval)
// sitemapHint multiplies by he4SitemapBoost when the sitemap lastmod is
// newer than our copy — a pre-fetch promotion, not ground truth.
func he4PriorityScore(importance, lambda, ageDays, intervalDays float64, sitemapHint bool) float64 {
	if lambda <= 0 {
		lambda = he4ChangeRateDefault
	}
	if intervalDays <= 0 {
		intervalDays = 120
	}
	if ageDays < 0 {
		ageDays = 0
	}
	expectedGain := 1 - math.Exp(-lambda*ageDays)
	overdue := ageDays / intervalDays
	p := importance * expectedGain * overdue
	if sitemapHint {
		p *= he4SitemapBoost
	}
	return p
}

// he4TightenFactor / he4BackoffFactor: the T3 multiplicative factors as
// functions of λ (design §4, "T3 interplay") instead of the old 0.75/1.25
// constants. Fast changers tighten harder (→×0.5); slow changers back off
// softer (→×1.1). At λ→0 the factors reproduce the legacy constants
// (0.75 / ~1.25), so behavior degrades gracefully to T3-classic.
func he4TightenFactor(lambda float64) float64 {
	if lambda < 0 {
		lambda = 0
	}
	return 0.5 + 0.25*math.Exp(-lambda)
}

func he4BackoffFactor(lambda float64) float64 {
	if lambda < 0 {
		lambda = 0
	}
	return 1.1 + 0.15*(1-math.Exp(-lambda))
}

// t3NextIntervalLambda is the Go reference implementation of the λ-aware T3
// interval update (parity-tested). NOTE: prod uses the SQL implementation in
// he4UpdateSignals; this exists for parity testing only (same split as
// t3NextInterval/adaptive.go).
func t3NextIntervalLambda(current float64, changed bool, lambda float64, def float64) float64 {
	if current <= 0 {
		current = def
	}
	if lambda <= 0 {
		lambda = he4ChangeRateDefault
	}
	if changed {
		if v := current * he4TightenFactor(lambda); v < t3IntervalFloorDays {
			return t3IntervalFloorDays
		} else {
			return v
		}
	}
	if v := current * he4BackoffFactor(lambda); v > t3IntervalCapDays {
		return t3IntervalCapDays
	} else {
		return v
	}
}

// he4Verdicts splits one flushed batch's successful (200) fetches by their
// SimHash-delta change verdict. Computed BEFORE the website_info upsert —
// after it, the stored fingerprint is already the fresh one.
type he4Verdicts struct {
	changed   []string          // 200, SimHash delta >= 8: observed change
	unchanged []string          // 200, SimHash delta < 8: observed no-change
	unknown   []string          // 200, fingerprint comparison impossible (legacy path)
	sitemap   map[string]time.Time // domain → newest sitemap lastmod seen this batch
}

// he4FetchVerdicts reads the stored SimHash fingerprints for the batch's good
// (200) domains and classifies each into changed/unchanged/unknown.
// Must run before the website_info upsert overwrites the stored fingerprints.
func he4FetchVerdicts(ctx context.Context, tx *sql.Tx, good []crawlOut) (*he4Verdicts, error) {
	v := &he4Verdicts{sitemap: map[string]time.Time{}}
	fps := make([]string, 0, len(good))
	fresh := map[string]uint64{}
	for _, b := range good {
		if !b.info.SitemapLastMod.IsZero() {
			if lm, ok := v.sitemap[b.domain]; !ok || b.info.SitemapLastMod.After(lm) {
				v.sitemap[b.domain] = b.info.SitemapLastMod
			}
		}
		if b.info.SimHash == 0 {
			v.unknown = append(v.unknown, b.domain)
			continue
		}
		fps = append(fps, b.domain)
		fresh[b.domain] = b.info.SimHash
	}
	stored := map[string]uint64{}
	if len(fps) > 0 {
		rows, err := tx.QueryContext(ctx,
			`SELECT domain, simhash FROM website_info WHERE domain = ANY($1)`, pq.Array(fps))
		if err != nil {
			return nil, fmt.Errorf("he4 stored simhashes: %w", err)
		}
		for rows.Next() {
			var d string
			var sh sql.NullInt64
			if err := rows.Scan(&d, &sh); err != nil {
				rows.Close()
				return nil, fmt.Errorf("he4 stored simhashes scan: %w", err)
			}
			if sh.Valid && sh.Int64 != 0 {
				stored[d] = uint64(sh.Int64)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("he4 stored simhashes rows: %w", err)
		}
	}
	for _, b := range good {
		fp, ok := fresh[b.domain]
		if !ok {
			continue // already in unknown
		}
		changed, known := he4ChangeObserved(false, fp, stored[b.domain])
		switch {
		case !known:
			v.unknown = append(v.unknown, b.domain)
		case changed:
			v.changed = append(v.changed, b.domain)
		default:
			v.unchanged = append(v.unchanged, b.domain)
		}
	}
	// Sorted domain order: same deadlock-avoidance discipline as the rest of
	// flushBatchTx — concurrent writers lock domain_info rows in the same order.
	sort.Strings(v.changed)
	sort.Strings(v.unchanged)
	sort.Strings(v.unknown)
	return v, nil
}

// he4UpdateSignals persists the HE-4 change signals for one flushed batch
// (design §§3–4). Runs inside the flush transaction, !skipStamp section only:
//
//  1. change_rate EWMA: observed change (SimHash delta >= 8) → obs=1;
//     observed no-change (304, or delta < 8) → obs=0. Unknown verdicts and
//     failures: no update (no signal).
//  2. T3 interval with λ-aware factors, driven by the SimHash verdict:
//     changed → tighten (harder for fast changers); unchanged → back off
//     (softer for slow changers). Unknown 200s keep the legacy tighten.
//  3. sitemap_lastmod: newest lastmod seen per domain (monotonic max).
func he4UpdateSignals(ctx context.Context, tx *sql.Tx, v *he4Verdicts, notmod []string, defInterval float64) error {
	// 1) change-rate EWMA. change_rate is updated first so the T3 factors
	// below read the fresh (post-observation, clamped) λ.
	if len(v.changed) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE domain_info
			SET change_rate = LEAST(GREATEST(COALESCE(change_rate, 1.0/120) * $2 + $3, 1.0/365), 2.0)
			WHERE domain = ANY($1)`,
			pq.Array(v.changed), 1-he4EWMAAlpha, he4EWMAAlpha); err != nil {
			return fmt.Errorf("he4 change_rate (changed): %w", err)
		}
	}
	unchangedObs := append(append([]string{}, v.unchanged...), notmod...)
	sort.Strings(unchangedObs)
	if len(unchangedObs) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE domain_info
			SET change_rate = LEAST(GREATEST(COALESCE(change_rate, 1.0/120) * $2, 1.0/365), 2.0)
			WHERE domain = ANY($1)`,
			pq.Array(unchangedObs), 1-he4EWMAAlpha); err != nil {
			return fmt.Errorf("he4 change_rate (unchanged): %w", err)
		}
	}

	// 2) T3 interval, λ-aware factors on the fresh change_rate.
	// changed (SimHash verdict, plus legacy unknown 200s) → tighten.
	tighten := append(append([]string{}, v.changed...), v.unknown...)
	sort.Strings(tighten)
	if len(tighten) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE domain_info
			SET web_crawled_at = now(),
			    recrawl_interval_days = GREATEST(LEAST(
			        COALESCE(recrawl_interval_days, $2) * (0.5 + 0.25*exp(-COALESCE(change_rate, 1.0/120))), 30), 1)
			WHERE domain = ANY($1)`,
			pq.Array(tighten), defInterval); err != nil {
			return fmt.Errorf("he4 t3 tighten: %w", err)
		}
	}
	// unchanged (304s + SimHash-verified-unchanged 200s) → back off, softer
	// for slow changers.
	if len(unchangedObs) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE domain_info
			SET web_crawled_at = now(),
			    recrawl_interval_days = LEAST(
			        COALESCE(recrawl_interval_days, $2) * (1.1 + 0.15*(1 - exp(-COALESCE(change_rate, 1.0/120)))), 30)
			WHERE domain = ANY($1)`,
			pq.Array(unchangedObs), defInterval); err != nil {
			return fmt.Errorf("he4 t3 backoff: %w", err)
		}
	}

	// 3) sitemap lastmod: monotonic max per domain (pre-fetch staleness hint).
	if len(v.sitemap) > 0 {
		doms := make([]string, 0, len(v.sitemap))
		for d := range v.sitemap {
			doms = append(doms, d)
		}
		sort.Strings(doms)
		var sb strings.Builder
		sb.WriteString(`UPDATE domain_info AS d SET sitemap_lastmod = GREATEST(COALESCE(d.sitemap_lastmod, '-infinity'::timestamptz), v.lm) FROM (VALUES `)
		args := make([]interface{}, 0, len(doms)*2)
		for i, dom := range doms {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "($%d,$%d)", i*2+1, i*2+2)
			args = append(args, dom, v.sitemap[dom])
		}
		sb.WriteString(`) AS v(domain, lm) WHERE d.domain = v.domain`)
		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("he4 sitemap_lastmod: %w", err)
		}
	}
	return nil
}

// he4DuePred is the recrawl-due predicate shared by the sweeper: a domain is
// due when its last attempt is older than its (T3-adapted, else global)
// interval, and its DNS isn't dead. $1 = global default interval (days).
const he4DuePred = `web_crawled_at < NOW() - make_interval(days => COALESCE(recrawl_interval_days, $1)) AND dns_fail_count < 3`

// he4ScoreExpr renders he4PriorityScore in SQL ($1 = default interval days).
// age_days is computed once per row from web_crawled_at.
const he4ScoreExpr = `(COALESCE(opic_history,0) + COALESCE(opic_cash,1.0))` +
	` * (1 - exp(-COALESCE(change_rate, 1.0/120) * (EXTRACT(EPOCH FROM (NOW() - web_crawled_at))/86400)))` +
	` * ((EXTRACT(EPOCH FROM (NOW() - web_crawled_at))/86400) / COALESCE(recrawl_interval_days, $1))` +
	` * (CASE WHEN sitemap_lastmod > web_crawled_at THEN 2.0 ELSE 1.0 END)`

// ensureHe4Columns adds the HE-4 columns when missing and builds the partial
// score index CONCURRENTLY. Follows the existing ensure pattern (opic/T3):
// idempotent, autocommit (CONCURRENTLY can't run in a transaction), and a
// rebuild if a previous CONCURRENTLY left the index INVALID.
func ensureHe4Columns(db *sql.DB) {
	for _, col := range []string{
		`ALTER TABLE domain_info ADD COLUMN IF NOT EXISTS change_rate REAL`,
		`ALTER TABLE domain_info ADD COLUMN IF NOT EXISTS next_score REAL`,
		`ALTER TABLE domain_info ADD COLUMN IF NOT EXISTS sitemap_lastmod TIMESTAMPTZ`,
	} {
		if _, err := db.Exec(col); err != nil {
			log.Printf("ensure he4 column: %v", err)
		}
	}
	var idxValid *bool
	_ = db.QueryRow(`SELECT indisvalid FROM pg_index WHERE indexrelid = 'idx_domain_info_next_score'::regclass`).Scan(&idxValid)
	if idxValid != nil && !*idxValid {
		log.Printf("ensure idx_domain_info_next_score: dropping INVALID index for rebuild")
		if _, err := db.Exec(`DROP INDEX idx_domain_info_next_score`); err != nil {
			log.Printf("ensure idx_domain_info_next_score drop: %v", err)
		}
		idxValid = nil
	}
	if idxValid == nil {
		if _, err := db.Exec(`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_domain_info_next_score
			ON domain_info (next_score DESC) WHERE next_score IS NOT NULL`); err != nil {
			log.Printf("ensure idx_domain_info_next_score: %v", err)
		}
	}
}

// refreshRecrawlScores is the HE-4 background sweeper (design §5): it
// materializes the priority score into domain_info.next_score so the hot draw
// is a partial-index range scan instead of a 20M-row sort.
//
// The score only grows with age (expected_gain → 1, overdue ↑), so the sweeper
// is bounded: newly-due rows get scored, scored-but-no-longer-due rows are
// cleared, and (full=true) the head of the queue — the rows the draw actually
// reads — gets its scores recomputed. full=false runs after each batch
// ingest; full=true on the HE4_SWEEP_S cadence (default 15 min).
func refreshRecrawlScores(ctx context.Context, db *sql.DB, defInterval float64, full bool) {
	t0 := time.Now()
	// 1) newly-due rows: NULL → P. Usually a small set (rows that aged past
	// their interval since the last sweep).
	res, err := db.ExecContext(ctx, `UPDATE domain_info SET next_score = `+he4ScoreExpr+
		` WHERE next_score IS NULL AND `+he4DuePred, defInterval)
	if err != nil {
		log.Printf("he4 sweep newly-due: %v", err)
		return
	}
	nNew, _ := res.RowsAffected()
	// 2) scored but no longer due (crawled since, or DNS-died) → clear.
	// Scans only the partial index (scored rows), not the whole table.
	res, err = db.ExecContext(ctx, `UPDATE domain_info SET next_score = NULL
		WHERE next_score IS NOT NULL AND NOT (`+he4DuePred+`)`, defInterval)
	if err != nil {
		log.Printf("he4 sweep cleanup: %v", err)
		return
	}
	nClear, _ := res.RowsAffected()
	nHead := int64(0)
	if full {
		// 3) head refresh: recompute P for the top-K scored rows, where
		// ordering accuracy matters (the draw reads the head). Bounded by
		// HE4_SCORE_REFRESH_CAP (default 100k).
		capN := 100000
		if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("HE4_SCORE_REFRESH_CAP"))); err == nil && v > 0 {
			capN = v
		}
		res, err = db.ExecContext(ctx, `UPDATE domain_info d SET next_score = `+he4ScoreExpr+`
			FROM (SELECT id FROM domain_info WHERE next_score IS NOT NULL ORDER BY next_score DESC LIMIT $2) h
			WHERE d.id = h.id AND `+he4DuePred, defInterval, capN)
		if err != nil {
			log.Printf("he4 sweep head-refresh: %v", err)
			return
		}
		nHead, _ = res.RowsAffected()
	}
	log.Printf("he4 sweep: scored %d newly-due, cleared %d, refreshed %d head rows in %s (full=%v)",
		nNew, nClear, nHead, time.Since(t0).Round(time.Millisecond), full)
}

// he4SweepLoop runs the full sweeper on the HE4_SWEEP_S cadence (default 900s
// = 15 min, design §5). Runs once immediately at startup so a fresh node
// scores its due set before the first draw.
func he4SweepLoop(ctx context.Context, db *sql.DB, defInterval float64) {
	cadence := 900
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("HE4_SWEEP_S"))); err == nil && v > 0 {
		cadence = v
	}
	refreshRecrawlScores(ctx, db, defInterval, true)
	t := time.NewTicker(time.Duration(cadence) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refreshRecrawlScores(ctx, db, defInterval, true)
		}
	}
}

// he4ClearScores clears next_score for just-crawled domains (they're no longer
// due). Called in the flush transaction right after stamping, so the queue
// never serves a domain we just fetched.
func he4ClearScores(ctx context.Context, tx *sql.Tx, domains []string) error {
	if len(domains) == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE domain_info SET next_score = NULL
		WHERE domain = ANY($1) AND next_score IS NOT NULL`, pq.Array(domains))
	return err
}
