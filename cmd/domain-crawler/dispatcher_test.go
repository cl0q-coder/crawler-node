package main

// Dispatcher acceptance tests (A2, #133):
//   1. M workers drawing URLs for ONE host still yield ≥2s gaps (the real floor) + ≤25/cycle.
//   2. Same-host requests are serialized; different hosts proceed in parallel.
//   3. Robots: fetched exactly once per host under concurrency, always honored.
//   4. Crawl-delay longer than host-delay wins.
//   5. Never lose a discovered domain — including results still in flight at shutdown.
//   go test -race ./cmd/domain-crawler

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRecorder struct {
	mu          sync.Mutex
	robotsCalls map[string]int
	pageStarts  map[string][]time.Time // per-host page-fetch start times
	pageURLs    []string
	pageDomains func(url string) []pendDomain // fabricated outcome domains per page URL
	pageSleep   time.Duration
	robotsRules *robotRules
}

func newFakeRecorder() *fakeRecorder {
	return &fakeRecorder{
		robotsCalls: map[string]int{},
		pageStarts:  map[string][]time.Time{},
		robotsRules: parseRobots("User-agent: *\nDisallow:\n"),
	}
}

func (f *fakeRecorder) fetch(job fetchJob) fetchResult {
	if job.kind == jobRobots {
		f.mu.Lock()
		f.robotsCalls[job.host]++
		f.mu.Unlock()
		return fetchResult{job: job, doneAt: time.Now(), robots: f.robotsRules}
	}
	f.mu.Lock()
	f.pageStarts[job.host] = append(f.pageStarts[job.host], time.Now())
	f.pageURLs = append(f.pageURLs, job.url)
	f.mu.Unlock()
	if f.pageSleep > 0 {
		time.Sleep(f.pageSleep)
	}
	oc := &pageOutcome{}
	if f.pageDomains != nil {
		oc.domains = f.pageDomains(job.url)
	}
	return fetchResult{job: job, doneAt: time.Now(), outcome: oc}
}

func testDispatcher(workers int, hostDelay time.Duration, perHostCap int, rec *fakeRecorder) (*dispatcher, *[][]pendDomain) {
	var mu sync.Mutex
	var flushed [][]pendDomain
	dis := newDispatcher(dispatchConfig{
		workers:    workers,
		hostDelay:  hostDelay,
		perHostCap: perHostCap,
		batchSize:  1000000, // tests flush explicitly unless noted
		maxDepth:   4,
		maxPages:   100000,
	}, rec.fetch, func(buf []pendDomain) int {
		mu.Lock()
		cp := append([]pendDomain(nil), buf...)
		flushed = append(flushed, cp)
		mu.Unlock()
		return len(buf)
	})
	dis.start()
	return dis, &flushed
}

func runCycle(t *testing.T, ctx context.Context, dis *dispatcher) {
	t.Helper()
	for !dis.drained() {
		if !dis.pump(ctx) {
			break
		}
	}
}

// minGap returns the smallest gap between consecutive timestamps (sorted).
func minGap(ts []time.Time) time.Duration {
	if len(ts) < 2 {
		return 0
	}
	cp := append([]time.Time(nil), ts...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Before(cp[j]) })
	m := cp[1].Sub(cp[0])
	for i := 2; i < len(cp); i++ {
		if g := cp[i].Sub(cp[i-1]); g < m {
			m = g
		}
	}
	return m
}

// TestDispatcherSingleHostGap: M workers, ONE host, the REAL 2s floor —
// consecutive fetches must stay ≥2s apart and robots must be fetched once.
func TestDispatcherSingleHostGap(t *testing.T) {
	rec := newFakeRecorder()
	dis, _ := testDispatcher(4, 2*time.Second, 25, rec)
	ctx := context.Background()
	dis.seed([]string{
		"http://one.example/a", "http://one.example/b", "http://one.example/c",
	})
	runCycle(t, ctx, dis)
	dis.shutdown()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if n := rec.robotsCalls["one.example"]; n != 1 {
		t.Errorf("robots.txt fetched %d times for one.example, want exactly 1", n)
	}
	starts := rec.pageStarts["one.example"]
	if len(starts) != 3 {
		t.Fatalf("page fetches = %d, want 3", len(starts))
	}
	if g := minGap(starts); g < 1900*time.Millisecond {
		t.Errorf("min inter-fetch gap = %v, want ≥ ~2s (the politeness floor)", g)
	}
}

// TestDispatcherPerHostCap: 30 URLs for one host, cap 25 — exactly 25 pages
// fetched, the rest dropped, gaps still respected.
func TestDispatcherPerHostCap(t *testing.T) {
	rec := newFakeRecorder()
	dis, _ := testDispatcher(8, 20*time.Millisecond, 25, rec)
	ctx := context.Background()
	var seeds []string
	for i := 0; i < 30; i++ {
		seeds = append(seeds, "http://cap.example/p"+string(rune('a'+i)))
	}
	dis.seed(seeds)
	runCycle(t, ctx, dis)
	dis.shutdown()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if n := len(rec.pageStarts["cap.example"]); n != 25 {
		t.Errorf("page fetches = %d, want exactly 25 (per-host cycle cap)", n)
	}
	if g := minGap(rec.pageStarts["cap.example"]); g < 10*time.Millisecond {
		t.Errorf("min inter-fetch gap = %v, want ≥ ~20ms", g)
	}
}

// TestDispatcherHostsParallel: different hosts proceed in parallel — 4 hosts
// × 1 page with 100ms fetches on 4 workers must cluster, not serialize.
func TestDispatcherHostsParallel(t *testing.T) {
	rec := newFakeRecorder()
	rec.pageSleep = 100 * time.Millisecond
	dis, _ := testDispatcher(4, 300*time.Millisecond, 25, rec)
	ctx := context.Background()
	dis.seed([]string{
		"http://h1.example/", "http://h2.example/", "http://h3.example/", "http://h4.example/",
	})
	start := time.Now()
	runCycle(t, ctx, dis)
	dis.shutdown()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var all []time.Time
	for _, ts := range rec.pageStarts {
		all = append(all, ts...)
	}
	if len(all) != 4 {
		t.Fatalf("page fetches = %d, want 4", len(all))
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Before(all[j]) })
	spread := all[len(all)-1].Sub(all[0])
	// Serialized they'd start ≥300ms apart (hostDelay); parallel they cluster.
	if spread > 250*time.Millisecond {
		t.Errorf("page start spread = %v, want < 250ms (parallel across hosts)", spread)
	}
	t.Logf("4 hosts × 100ms fetches completed in %v", time.Since(start))
}

// TestDispatcherRobotsSingleflight: 8 workers × 10 URLs on one host —
// robots.txt fetched exactly once, and disallowed paths never fetched.
func TestDispatcherRobotsSingleflight(t *testing.T) {
	rec := newFakeRecorder()
	rec.robotsRules = parseRobots("User-agent: *\nDisallow: /private/\n")
	dis, _ := testDispatcher(8, 10*time.Millisecond, 25, rec)
	ctx := context.Background()
	var seeds []string
	for i := 0; i < 5; i++ {
		seeds = append(seeds, "http://sf.example/page"+string(rune('0'+i)))
		seeds = append(seeds, "http://sf.example/private/s"+string(rune('0'+i)))
	}
	dis.seed(seeds)
	runCycle(t, ctx, dis)
	dis.shutdown()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if n := rec.robotsCalls["sf.example"]; n != 1 {
		t.Errorf("robots.txt fetched %d times, want exactly 1 (singleflight)", n)
	}
	for _, u := range rec.pageURLs {
		if strings.Contains(u, "/private/") {
			t.Errorf("robots-disallowed URL was fetched: %s", u)
		}
	}
	if n := len(rec.pageURLs); n != 5 {
		t.Errorf("allowed page fetches = %d, want 5", n)
	}
}

// TestDispatcherCrawlDelayWins: a Crawl-delay longer than host-delay governs
// the gap.
func TestDispatcherCrawlDelayWins(t *testing.T) {
	rec := newFakeRecorder()
	rec.robotsRules = parseRobots("User-agent: *\nDisallow:\nCrawl-delay: 1\n")
	dis, _ := testDispatcher(4, 50*time.Millisecond, 25, rec)
	ctx := context.Background()
	dis.seed([]string{"http://cd.example/a", "http://cd.example/b"})
	runCycle(t, ctx, dis)
	dis.shutdown()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	starts := rec.pageStarts["cd.example"]
	if len(starts) != 2 {
		t.Fatalf("page fetches = %d, want 2", len(starts))
	}
	if g := minGap(starts); g < 900*time.Millisecond {
		t.Errorf("min inter-fetch gap = %v, want ≥ ~1s (Crawl-delay beats host-delay)", g)
	}
}

// TestDispatcherNeverLosesDomains: every unique discovered domain reaches the
// flusher exactly once, including the final partial batch.
func TestDispatcherNeverLosesDomains(t *testing.T) {
	rec := newFakeRecorder()
	n := 0
	rec.pageDomains = func(pageURL string) []pendDomain {
		n++
		return []pendDomain{
			{domain: "shared.example", foundOn: pageURL}, // duplicate across pages: deduped
			{domain: "unique-n.example", foundOn: pageURL},
		}
	}
	dis, flushed := testDispatcher(4, 10*time.Millisecond, 25, rec)
	dis.cfg.batchSize = 3 // small batches: forces multiple flushes + a partial tail
	ctx := context.Background()
	dis.seed([]string{
		"http://dl.example/1", "http://dl.example/2", "http://dl.example/3", "http://dl.example/4",
	})
	runCycle(t, ctx, dis)
	dis.shutdown() // flushes the tail

	seen := map[string]int{}
	for _, b := range *flushed {
		for _, pd := range b {
			seen[pd.domain]++
		}
	}
	if got := seen["shared.example"]; got != 1 {
		t.Errorf("shared.example flushed %d times, want exactly 1", got)
	}
	if got := seen["unique-n.example"]; got != 1 {
		t.Errorf("unique-n.example flushed %d times, want exactly 1", got)
	}
	if len(seen) != 2 {
		t.Errorf("flushed %d distinct domains, want 2", len(seen))
	}
	if dis.discovered != 2 {
		t.Errorf("discovered = %d, want 2", dis.discovered)
	}
}

// TestDispatcherShutdownDrainsInflight: results still in flight when the
// context is cancelled are folded in and flushed on shutdown — a 429'd (or
// any) domain is never lost.
func TestDispatcherShutdownDrainsInflight(t *testing.T) {
	rec := newFakeRecorder()
	rec.pageSleep = 500 * time.Millisecond // fetches still running when we cancel
	rec.pageDomains = func(pageURL string) []pendDomain {
		return []pendDomain{{domain: "inflight.example", foundOn: pageURL}}
	}
	dis, flushed := testDispatcher(2, 10*time.Millisecond, 25, rec)
	ctx, cancel := context.WithCancel(context.Background())
	dis.seed([]string{"http://sd.example/1", "http://sd.example/2"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !dis.drained() {
			if !dis.pump(ctx) {
				break
			}
		}
	}()
	time.Sleep(100 * time.Millisecond) // let both page jobs go in flight
	cancel()
	<-done
	dis.shutdown()

	total := 0
	for _, b := range *flushed {
		total += len(b)
	}
	if total != 1 { // both pages found the same domain → deduped to one row
		t.Errorf("flushed %d domains after shutdown, want 1 (deduped in-flight result)", total)
	}
	if dis.inFlight != 0 {
		t.Errorf("inFlight = %d after shutdown, want 0", dis.inFlight)
	}
}
