// HE-6: unknown-domain frontier sweep — probe reserve, prioritization, draw.
package main

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"testing"
	"time"
)

// tripBreakerForTest opens the breaker for key (threshold 1 → a single fail
// trips it for an hour) and restores the breaker afterwards.
func tripBreakerForTest(t *testing.T, key string) {
	t.Helper()
	oldEnabled, oldThreshold := breaker.enabled, breaker.threshold
	oldWindow, oldBase, oldCap := breaker.window, breaker.base, breaker.cap
	breaker.enabled = true
	breaker.threshold = 1
	breaker.window = time.Minute
	breaker.base = time.Hour
	breaker.cap = 2 * time.Hour
	breaker.fail(key)
	if !breaker.blocked(key) {
		t.Fatalf("breaker not blocked for %s after fail", key)
	}
	t.Cleanup(func() {
		breaker.enabled, breaker.threshold = oldEnabled, oldThreshold
		breaker.window, breaker.base, breaker.cap = oldWindow, oldBase, oldCap
		breaker.m.Delete(key)
	})
}

func TestProbeReserve(t *testing.T) {
	t.Setenv("FRONTIER_PROBE_PCT", "1")
	cases := []struct {
		maxReturn, want int
	}{
		{1000, 10}, // 1% of the production batch
		{100, 1},
		{50, 1}, // minimum 1 when enabled
		{1, 0},  // tiny batch: reserve must not dominate
		{2, 1},  // 2/2 = 1 → not clamped (r=1 <= 1)
		{0, 0},
		{-5, 0},
	}
	for _, c := range cases {
		if got := probeReserve(c.maxReturn); got != c.want {
			t.Errorf("probeReserve(%d) = %d, want %d", c.maxReturn, got, c.want)
		}
	}
}

func TestProbeReserveDisabled(t *testing.T) {
	t.Setenv("FRONTIER_PROBE_PCT", "0")
	if got := probeReserve(1000); got != 0 {
		t.Errorf("probeReserve(1000) with PCT=0 = %d, want 0 (kill switch)", got)
	}
}

func TestProbeReservePct(t *testing.T) {
	t.Setenv("FRONTIER_PROBE_PCT", "5")
	if got := probeReserve(1000); got != 50 {
		t.Errorf("probeReserve(1000) with PCT=5 = %d, want 50", got)
	}
}

// TestCapBatchProbeBypass: pre-marked probes (from the snapshot draw) are
// admitted even though their group is breaker-open; unmarked blocked rows
// stay deferred. draw() is the only probe source — capBatch never invents
// probes on its own.
func TestCapBatchProbeBypass(t *testing.T) {
	t.Setenv("FRONTIER_PROBE_PCT", "1")
	tripBreakerForTest(t, "asn:BadASN")
	var ts []target
	for i := 0; i < 10; i++ {
		ts = append(ts, target{domain: fmt.Sprintf("b%d.bad.example", i), org: "BadASN"})
	}
	ts[0].probe, ts[1].probe, ts[2].probe = true, true, true
	got := capBatch(ts, 25, 100)
	if len(got) != 3 {
		t.Fatalf("capBatch admitted %d, want exactly the 3 marked probes", len(got))
	}
	for _, g := range got {
		if !g.probe {
			t.Errorf("admitted %s from blocked group without probe mark", g.domain)
		}
	}
}

// TestCapBatchHealthyUnchanged: a healthy pool still yields the full batch
// with zero probes — the reserve only exists when draw() marks rows.
func TestCapBatchHealthyUnchanged(t *testing.T) {
	t.Setenv("FRONTIER_PROBE_PCT", "1")
	var ts []target
	for i := 0; i < 200; i++ {
		ts = append(ts, target{domain: fmt.Sprintf("h%d.example-%d.org", i, i), org: fmt.Sprintf("HealthyASN%d", i)})
	}
	got := capBatch(ts, 0, 100)
	if len(got) != 100 {
		t.Fatalf("capBatch admitted %d, want full batch 100", len(got))
	}
	for _, g := range got {
		if g.probe {
			t.Errorf("healthy pool produced probe mark on %s", g.domain)
		}
	}
}

// TestCapBatchUnknownsOutrankRecrawls: fresh rows are passed before stale
// rows and admission follows input order, so unknowns fill the batch before
// any recrawl is admitted.
func TestCapBatchUnknownsOutrankRecrawls(t *testing.T) {
	t.Setenv("FRONTIER_PROBE_PCT", "1")
	var ts []target
	for i := 0; i < 1500; i++ {
		ts = append(ts, target{domain: fmt.Sprintf("fresh%d.example", i), org: fmt.Sprintf("FreshASN%d", i)})
	}
	for i := 0; i < 1500; i++ {
		ts = append(ts, target{domain: fmt.Sprintf("stale%d.example", i), org: fmt.Sprintf("StaleASN%d", i)})
	}
	got := capBatch(ts, 0, 1000)
	if len(got) != 1000 {
		t.Fatalf("capBatch admitted %d, want 1000", len(got))
	}
	for _, g := range got {
		if len(g.domain) < 5 || g.domain[:5] != "fresh" {
			t.Fatalf("recrawl %s admitted while %d fresh rows were still queued — unknowns must outrank recrawls", g.domain, 1500-len(got))
		}
	}
}

// TestCapBatchDrawOutputShape: the shape draw() actually emits — closed rows
// within the main budget, then marked probes — is admitted whole by capBatch:
// unknowns (including the canary) outrank recrawls and nothing is lost.
func TestCapBatchDrawOutputShape(t *testing.T) {
	t.Setenv("FRONTIER_PROBE_PCT", "1")
	tripBreakerForTest(t, "asn:BadASN")
	var ts []target
	for i := 0; i < 990; i++ {
		ts = append(ts, target{domain: fmt.Sprintf("fresh%d.example", i), org: fmt.Sprintf("FreshASN%d", i)})
	}
	for i := 0; i < 10; i++ {
		ts = append(ts, target{domain: fmt.Sprintf("b%d.bad.example", i), org: "BadASN", probe: true})
	}
	for i := 0; i < 1500; i++ {
		ts = append(ts, target{domain: fmt.Sprintf("stale%d.example", i), org: fmt.Sprintf("StaleASN%d", i)})
	}
	got := capBatch(ts, 0, 1000)
	if len(got) != 1000 {
		t.Fatalf("capBatch admitted %d, want 1000", len(got))
	}
	var probes int
	for _, g := range got {
		if g.probe {
			probes++
		} else if len(g.domain) < 5 || g.domain[:5] != "fresh" {
			t.Fatalf("recrawl %s admitted before fresh rows were exhausted", g.domain)
		}
	}
	if probes != probeReserve(1000) {
		t.Errorf("got %d probes, want reserve %d", probes, probeReserve(1000))
	}
}

// TestDrawAllBlockedYieldsProbes is the HE-6 defer-forever killer at the
// layer that owns it: a snapshot whose EVERY group is breaker-open must
// still yield probe targets — otherwise those rows are never attempted,
// never stamped, and pile up forever.
func TestDrawAllBlockedYieldsProbes(t *testing.T) {
	t.Setenv("FRONTIER_PROBE_PCT", "1")
	tripBreakerForTest(t, "asn:BadASN")
	var f frontierSnapshot
	for i := 0; i < 200; i++ {
		f.rows = append(f.rows, target{domain: fmt.Sprintf("b%d.bad.example", i), org: "BadASN"})
		f.keys = append(f.keys, "asn:BadASN")
	}
	f.loaded = time.Now()
	// max=400: 200 rows is not < max/2 → no reload → db unused (nil ok).
	got, err := f.draw(nil, 100, 50, time.Hour, 400)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("draw returned 0 targets for a fully-blocked snapshot: defer-forever not fixed")
	}
	if want := probeReserve(100); len(got) != want {
		t.Errorf("draw returned %d targets, want exactly the probe reserve %d", len(got), want)
	}
	for _, g := range got {
		if !g.probe {
			t.Errorf("draw returned %s from blocked group without probe mark", g.domain)
		}
	}
}

// TestDrawTwoPass: draw serves closed-group rows first, then the probe
// reserve from breaker-open groups, marked probe.
func TestDrawTwoPass(t *testing.T) {
	t.Setenv("FRONTIER_PROBE_PCT", "1")
	tripBreakerForTest(t, "asn:BadASN")
	var f frontierSnapshot
	for i := 0; i < 60; i++ {
		f.rows = append(f.rows, target{domain: fmt.Sprintf("g%d.good.example", i), org: "GoodASN"})
		f.keys = append(f.keys, "asn:GoodASN")
	}
	for i := 0; i < 60; i++ {
		f.rows = append(f.rows, target{domain: fmt.Sprintf("b%d.bad.example", i), org: "BadASN"})
		f.keys = append(f.keys, "asn:BadASN")
	}
	f.loaded = time.Now()
	// max=200: 120 rows is not < max/2 → no reload → db unused (nil ok).
	got, err := f.draw(nil, 100, 50, time.Hour, 200)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	var probes int
	for _, g := range got {
		if g.probe {
			probes++
			if g.groupKey() != "asn:BadASN" {
				t.Errorf("probe %s from unexpected group %s", g.domain, g.groupKey())
			}
		} else if g.groupKey() != "asn:GoodASN" {
			t.Errorf("non-probe %s from blocked group %s", g.domain, g.groupKey())
		}
	}
	if probes != probeReserve(100) {
		t.Errorf("draw produced %d probes, want reserve %d", probes, probeReserve(100))
	}
	// Closed rows came first (input order): the first len(got)-probes are GoodASN.
	for i := 0; i < len(got)-probes; i++ {
		if got[i].groupKey() != "asn:GoodASN" {
			t.Errorf("draw[%d] = %s before closed rows were exhausted", i, got[i].domain)
		}
	}
}

// TestDrawAdaptiveReload: a more-than-half-drawn snapshot reloads even when
// the timed refresh hasn't elapsed (keeps the fresh pipeline fed under fast
// drain). Uses an unconnectable DB: the reload attempt surfaces as an error,
// which proves the reload was attempted.
func TestDrawAdaptiveReload(t *testing.T) {
	t.Setenv("FRONTIER_PROBE_PCT", "0") // simplify: no probes
	db, _ := sql.Open("postgres", "postgres://127.0.0.1:1/nonexistent?sslmode=disable&connect_timeout=1")
	defer db.Close()
	var f frontierSnapshot
	for i := 0; i < 120; i++ {
		f.rows = append(f.rows, target{domain: fmt.Sprintf("d%d.example", i), org: fmt.Sprintf("ASN%d", i)})
		f.keys = append(f.keys, fmt.Sprintf("asn:ASN%d", i))
	}
	f.loaded = time.Now()
	// 120 rows, max=200 → 120 < 100 is false → no reload on the first draw.
	if _, err := f.draw(db, 70, 0, time.Hour, 200); err != nil {
		t.Fatalf("first draw: %v", err)
	}
	// 50 rows left < max/2=100 → the next draw must attempt a reload.
	if _, err := f.draw(db, 70, 0, time.Hour, 200); err == nil {
		t.Error("second draw should have attempted a reload (snapshot more than half drawn)")
	}
}

// BenchmarkDrawTwoPass measures the HE-6 two-pass draw over a 60k-row
// snapshot (mixed open/closed groups): the per-select cost of the probe
// reserve on the deployed snapshot path.
func BenchmarkDrawTwoPass(b *testing.B) {
	b.Setenv("FRONTIER_PROBE_PCT", "1")
	oldEnabled, oldThreshold := breaker.enabled, breaker.threshold
	oldWindow, oldBase, oldCap := breaker.window, breaker.base, breaker.cap
	breaker.enabled, breaker.threshold = true, 1
	breaker.window, breaker.base, breaker.cap = time.Minute, time.Hour, 2*time.Hour
	breaker.fail("asn:BadASN")
	b.Cleanup(func() {
		breaker.enabled, breaker.threshold = oldEnabled, oldThreshold
		breaker.window, breaker.base, breaker.cap = oldWindow, oldBase, oldCap
		breaker.m.Delete("asn:BadASN")
	})
	newSnap := func() *frontierSnapshot {
		f := &frontierSnapshot{}
		for i := 0; i < 30000; i++ {
			f.rows = append(f.rows, target{domain: fmt.Sprintf("g%d.good.example", i), org: "GoodASN"})
			f.keys = append(f.keys, "asn:GoodASN")
		}
		for i := 0; i < 30000; i++ {
			f.rows = append(f.rows, target{domain: fmt.Sprintf("b%d.bad.example", i), org: "BadASN"})
			f.keys = append(f.keys, "asn:BadASN")
		}
		f.loaded = time.Now()
		return f
	}
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		f := newSnap()
		b.StartTimer()
		got, err := f.draw(nil, 1000, 25, time.Hour, 100000)
		if err != nil || len(got) == 0 {
			b.Fatalf("draw: %v, %d targets", err, len(got))
		}
	}
}
