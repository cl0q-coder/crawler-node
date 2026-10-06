package main

import (
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"cl0q/internal/tunable"
)

// NR7: property-style etiquette conformance. These hammer the politeness
// invariants directly: min delays hold under concurrency, Crawl-delay wins
// over the configured floor, per-group caps bind, and breaker backoff has
// the right shape (doubling, capped, recoverable).

// TestThrottleEnforcesMinDelayUnderConcurrency: N goroutines racing on one
// key must be serialized with at least `delay` between acquisitions.
func init() {
	if tunables == nil {
		tunables = tunable.New(time.Second, time.Second, 50)
	}
}

func TestThrottleEnforcesMinDelayUnderConcurrency(t *testing.T) {
	var gates sync.Map
	const n = 6
	const delay = 40 * time.Millisecond
	const tolerance = 15 * time.Millisecond

	var mu sync.Mutex
	times := make([]time.Time, 0, n)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			throttle(&gates, "nr7-key", delay)
			mu.Lock()
			times = append(times, time.Now())
			mu.Unlock()
		}()
	}
	wg.Wait()

	if elapsed := time.Since(start); elapsed < time.Duration(n-1)*delay {
		t.Errorf("total %s < minimum %s for %d serialized acquisitions", elapsed, time.Duration(n-1)*delay, n)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap < delay-tolerance {
			t.Errorf("acquisitions %d→%d only %s apart, want >= %s", i-1, i, gap, delay)
		}
	}
}

// TestThrottleIndependentKeysDontBlock: different keys proceed in parallel.
func TestThrottleIndependentKeysDontBlock(t *testing.T) {
	var gates sync.Map
	const delay = 200 * time.Millisecond
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			throttle(&gates, fmt.Sprintf("nr7-par-%d", i), delay)
			throttle(&gates, fmt.Sprintf("nr7-par-%d", i), delay)
		}(i)
	}
	wg.Wait()
	// Each key is hit twice serially (2*delay), but keys run in parallel,
	// so the whole thing must finish well under 4 keys * 2 * delay.
	if elapsed := time.Since(start); elapsed > 3*delay {
		t.Errorf("independent keys blocked each other: %s for 4 parallel keys", elapsed)
	}
}

// TestSitePaceObeysLongerCrawlDelay: a site's Crawl-delay longer than the
// configured host floor must win (max, not the floor).
func TestSitePaceObeysLongerCrawlDelay(t *testing.T) {
	old := tunables.HostDelay()
	tunables.SetHostDelayForTest(20 * time.Millisecond)
	defer func() { tunables.SetHostDelayForTest(old) }()

	host := "nr7-crawldelay.example"
	sitePace(host, 150*time.Millisecond)
	start := time.Now()
	sitePace(host, 150*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond-20*time.Millisecond {
		t.Errorf("Crawl-delay not obeyed: second pace took %s, want >= ~150ms", elapsed)
	}
}

// TestSitePaceFloorAppliesWhenNoCrawlDelay: with no Crawl-delay the
// configured host floor still paces.
func TestSitePaceFloorAppliesWhenNoCrawlDelay(t *testing.T) {
	old := tunables.HostDelay()
	tunables.SetHostDelayForTest(80 * time.Millisecond)
	defer func() { tunables.SetHostDelayForTest(old) }()

	host := "nr7-floor.example"
	sitePace(host, 0)
	start := time.Now()
	sitePace(host, 0)
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond-20*time.Millisecond {
		t.Errorf("host floor not enforced: second pace took %s, want >= ~80ms", elapsed)
	}
}

// TestCapBatchEnforcesPerGroupCap: no group contributes more than
// maxPerGroup targets, no matter how many it has in the pool.
func TestCapBatchEnforcesPerGroupCap(t *testing.T) {
	var ts []target
	for i := 0; i < 60; i++ {
		ts = append(ts, target{domain: fmt.Sprintf("d%d.example.com", i), org: "ExampleASN"})
	}
	for i := 0; i < 10; i++ {
		ts = append(ts, target{domain: fmt.Sprintf("o%d.other.net", i), org: "OtherASN"})
	}
	got := capBatch(ts, 5, 100)
	counts := map[string]int{}
	for _, g := range got {
		counts[g.groupKey()]++
	}
	for k, c := range counts {
		if c > 5 {
			t.Errorf("group %s contributed %d targets, want <= 5", k, c)
		}
	}
	if counts["asn:ExampleASN"] != 5 {
		t.Errorf("ExampleASN contributed %d, want exactly 5 (cap binds)", counts["asn:ExampleASN"])
	}
}

// TestCapBatchScoreOrder: admission follows the pool's score order (OPIC
// priority, #105). The pool arrives OPIC-score-ordered from the DB; capBatch
// admits the top maxPerGroup per group deterministically. De-clustering is
// preserved by the cap, not by shuffling.
// TestCapBatchTurnover: the tail is not starved forever — once admitted
// targets leave the pool (crawled and stamped), the next tier is admitted on
// the following pass. Anti-starvation is a system-level property (pool
// turnover per batch), not a per-batch shuffle.
func TestCapBatchScoreOrder(t *testing.T) {
	var ts []target
	for i := 0; i < 40; i++ {
		ts = append(ts, target{domain: fmt.Sprintf("s%d.example.com", i), org: "ScoreASN"})
	}
	got := capBatch(ts, 5, 100)
	if len(got) != 5 {
		t.Fatalf("capBatch admitted %d, want 5 (cap binds)", len(got))
	}
	for i, g := range got {
		want := fmt.Sprintf("s%d.example.com", i)
		if g.domain != want {
			t.Errorf("admitted[%d] = %s, want %s (score order)", i, g.domain, want)
		}
	}
}

func TestCapBatchTurnover(t *testing.T) {
	var ts []target
	for i := 0; i < 40; i++ {
		ts = append(ts, target{domain: fmt.Sprintf("s%d.example.com", i), org: "TurnoverASN"})
	}
	// First pass admits s0..s4; simulate them leaving the pool (crawled).
	first := capBatch(ts, 5, 100)
	admitted := map[string]bool{}
	for _, g := range first {
		admitted[g.domain] = true
	}
	var rest []target
	for _, x := range ts {
		if !admitted[x.domain] {
			rest = append(rest, x)
		}
	}
	second := capBatch(rest, 5, 100)
	if len(second) != 5 || second[0].domain != "s5.example.com" {
		t.Errorf("second pass admitted %v, want s5..s9 (tail advances)", second)
	}
}

// TestBreakerBackoffShape: cooldown doubles per consecutive trip and pins
// at the cap instead of growing forever.
func TestBreakerBackoffShape(t *testing.T) {
	b := &asnBreaker{enabled: true, threshold: 2, window: time.Hour, base: 10 * time.Millisecond, cap: 40 * time.Millisecond}
	key := "nr7-shape"

	trip := func() time.Duration {
		b.fail(key)
		b.fail(key) // threshold=2 → trips
		v, _ := b.m.Load(key)
		s := v.(*breakState)
		s.mu.Lock()
		defer s.mu.Unlock()
		return time.Until(s.openUntil)
	}
	// Reset the open window between trips so fail() doesn't early-return.
	reset := func() {
		v, _ := b.m.Load(key)
		s := v.(*breakState)
		s.mu.Lock()
		s.openUntil = time.Now().Add(-time.Second)
		s.mu.Unlock()
	}

	d1 := trip()
	reset()
	d2 := trip()
	reset()
	d3 := trip() // would be 80ms without the cap

	if d1 < 9*time.Millisecond || d1 > 12*time.Millisecond {
		t.Errorf("first cooldown = %s, want ~10ms (base)", d1)
	}
	if d2 < 18*time.Millisecond || d2 > 24*time.Millisecond {
		t.Errorf("second cooldown = %s, want ~20ms (doubled)", d2)
	}
	if d3 < 38*time.Millisecond || d3 > 45*time.Millisecond {
		t.Errorf("third cooldown = %s, want ~40ms (capped, not 80ms)", d3)
	}
}

// TestBreakerRecoversAfterSuccess: a clean result decays the escalation
// level so a recovered group isn't punished forever.
func TestBreakerRecoversAfterSuccess(t *testing.T) {
	b := &asnBreaker{enabled: true, threshold: 1, window: time.Hour, base: 10 * time.Millisecond, cap: time.Hour}
	key := "nr7-recover"

	b.fail(key) // trips immediately (threshold=1), level 1, ~10ms
	v, _ := b.m.Load(key)
	s := v.(*breakState)
	s.mu.Lock()
	levelAfterTrip := s.level
	s.openUntil = time.Now().Add(-time.Second) // let the cooldown lapse
	s.mu.Unlock()
	if levelAfterTrip != 1 {
		t.Fatalf("level after trip = %d, want 1", levelAfterTrip)
	}
	b.success(key) // clean result past the cooldown → level decays
	s.mu.Lock()
	levelAfterSuccess := s.level
	s.mu.Unlock()
	if levelAfterSuccess != 0 {
		t.Errorf("level after clean success = %d, want 0 (decayed)", levelAfterSuccess)
	}
}
