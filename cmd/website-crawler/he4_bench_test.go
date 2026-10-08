package main

// Benchmarks for the HE-4 freshness/scoring hot path.
// Run: go test -run xxx -bench 'BenchmarkHe4' -benchmem ./cmd/website-crawler/
//
// NOTE on the SQL draw: selectFromSnapshot's recrawl draw is now
//   SELECT ... FROM domain_info WHERE next_score IS NOT NULL
//   ORDER BY next_score DESC LIMIT $1
// served by the partial index idx_domain_info_next_score — a pure index
// range scan touching only LIMIT rows, versus the old
//   WHERE web_crawled_at < ... AND dns_fail_count < 3 LIMIT $2
// which scanned the due set through idx_domain_info_web_crawled_at.
// No Postgres is available in this environment to measure the draw
// directly; the plan shape (Index Scan using idx_domain_info_next_score,
// no sort node) should be confirmed with EXPLAIN on prod-scale data at
// deploy time. The Go-side scoring math below is the only new per-row CPU
// on the hot path, and it runs in the background sweeper, not the draw.

import (
	"testing"
)

func BenchmarkHe4PriorityScore(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// Representative: mid-importance weekly changer, 10 days old.
		_ = he4PriorityScore(25.0, 1.0/7, 10.0, 7.0, false)
	}
}

func BenchmarkHe4PriorityScoreHinted(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = he4PriorityScore(25.0, 1.0/7, 10.0, 7.0, true)
	}
}

func BenchmarkHe4ChangeObserved(b *testing.B) {
	b.ReportAllocs()
	fresh, stored := uint64(0xAAAAAAAAAAAAAAAA), uint64(0xAAAAAAAAAAAAAA55)
	for i := 0; i < b.N; i++ {
		_, _ = he4ChangeObserved(false, fresh, stored)
	}
}

func BenchmarkHe4NewChangeRate(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = he4NewChangeRate(0.08, true)
	}
}

func BenchmarkT3NextIntervalLambda(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = t3NextIntervalLambda(7.0, true, 0.5, 120.0)
	}
}
