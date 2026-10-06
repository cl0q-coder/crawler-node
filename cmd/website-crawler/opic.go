package main

// T2: OPIC scoring (streaming PageRank approximation).
//
// Each domain holds "cash" (float). When a page is crawled, its cash is
// distributed equally among its outlink domains, its own cash resets to zero,
// and the spent cash accumulates into "history". Score = history + cash; the
// frontier orders by score DESC. O(1) state per domain, zero extra fetches.
// Paper: Abiteboul et al., "Adaptive On-Line Page Importance Computation", WWW 2003.
//
// New domains start with cash=1.0 (the OPIC initial value). Domains discovered
// as outlinks that don't exist yet are inserted with the default; the
// distributed share only lands on existing rows (a deliberate v1 simplification).

import (
	"context"
	"sort"

	"database/sql"
	"github.com/lib/pq"
)

// opicShares computes the per-destination cash shares for OPIC distribution.
// sources maps crawled domain -> its outlink domains; cash maps domain -> current cash.
// Returns (dest -> total share) aggregated across all sources. Pure function, unit-tested.
func opicShares(sources map[string][]string, cash map[string]float64) map[string]float64 {
	out := make(map[string]float64)
	// Deterministic iteration for stable test expectations.
	sorted := make([]string, 0, len(sources))
	for s := range sources {
		sorted = append(sorted, s)
	}
	sort.Strings(sorted)
	for _, src := range sorted {
		outs := sources[src]
		if len(outs) == 0 {
			continue
		}
		c := cash[src] // missing row → 0, no distribution
		if c <= 0 {
			continue
		}
		per := c / float64(len(outs))
		for _, dest := range outs {
			if dest == src {
				continue // no self-payment
			}
			out[dest] += per
		}
	}
	return out
}

// opicDistribute runs the OPIC cash distribution for a batch of crawled domains.
// sources maps crawled domain -> its outlink domains (from outDomains).
// It must run inside the caller's transaction (tx).
func opicDistribute(ctx context.Context, tx *sql.Tx, sources map[string][]string) error {
	if len(sources) == 0 {
		return nil
	}
	domains := make([]string, 0, len(sources))
	for d := range sources {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	// Fetch current cash for the crawled domains.
	rows, err := tx.QueryContext(ctx,
		`SELECT domain, opic_cash FROM domain_info WHERE domain = ANY($1)`, pq.Array(domains))
	if err != nil {
		return err
	}
	cash := make(map[string]float64, len(domains))
	for rows.Next() {
		var d string
		var c float64
		if err := rows.Scan(&d, &c); err != nil {
			rows.Close()
			return err
		}
		cash[d] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	shares := opicShares(sources, cash)
	if len(shares) > 0 {
		dests := make([]string, 0, len(shares))
		amts := make([]float64, 0, len(shares))
		for d, a := range shares {
			dests = append(dests, d)
			amts = append(amts, a)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE domain_info di SET opic_cash = di.opic_cash + t.total
			FROM (SELECT d, SUM(a) AS total FROM unnest($1::text[], $2::float[]) AS t(d, a) GROUP BY d) t
			WHERE di.domain = t.d`, pq.Array(dests), pq.Array(amts)); err != nil {
			return err
		}
	}

	// Reset crawled domains: history += cash, cash = 0.
	if _, err := tx.ExecContext(ctx,
		`UPDATE domain_info SET opic_history = opic_history + opic_cash, opic_cash = 0 WHERE domain = ANY($1)`,
		pq.Array(domains)); err != nil {
		return err
	}
	return nil
}
