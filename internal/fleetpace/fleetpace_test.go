package fleetpace

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"
)

// HE-7: the N-node contract. N simulated nodes share ONE SimShared pacer (the
// stand-in for the future central bucket). Every node's requests to the host
// must be spaced ≥ the host's Crawl-delay in AGGREGATE — the fleet behaves as
// one well-behaved crawler.
func TestSimSharedEnforcesCrawlDelayAcrossNodes(t *testing.T) {
	const nodes = 8
	const perNode = 6
	const crawlDelay = 60 * time.Millisecond
	const tolerance = 20 * time.Millisecond

	shared := NewSimShared()
	shared.ReportCrawlDelay("example.com", crawlDelay)

	var mu sync.Mutex
	stamps := make([]time.Time, 0, nodes*perNode)
	var wg sync.WaitGroup
	for n := 0; n < nodes; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perNode; i++ {
				// Nodes pace at the fleet floor; the host's Crawl-delay comes
				// only from the shared learned value — as if this node never
				// fetched robots.txt itself.
				if err := shared.Wait(context.Background(), "example.com", 0); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				stamps = append(stamps, time.Now())
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(stamps) != nodes*perNode {
		t.Fatalf("got %d requests, want %d", len(stamps), nodes*perNode)
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i].Before(stamps[j]) })
	for i := 1; i < len(stamps); i++ {
		if gap := stamps[i].Sub(stamps[i-1]); gap < crawlDelay-tolerance {
			t.Fatalf("fleet-wide gap %d→%d was %s, want >= ~%s: %d nodes did not behave as one crawler",
				i-1, i, gap, crawlDelay, nodes)
		}
	}
}

// The same workload against N INDEPENDENT local pacers (today's single-node
// behavior) must overshoot — this is the DDoS the shared limiter fixes, and
// it proves the test above isn't vacuous.
func TestIndependentLocalsOvershootCrawlDelay(t *testing.T) {
	const nodes = 8
	const perNode = 6
	const crawlDelay = 60 * time.Millisecond

	var mu sync.Mutex
	stamps := make([]time.Time, 0, nodes*perNode)
	var wg sync.WaitGroup
	for n := 0; n < nodes; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := NewLocal() // each node paces only itself
			for i := 0; i < perNode; i++ {
				if err := local.Wait(context.Background(), "example.com", crawlDelay); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				stamps = append(stamps, time.Now())
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	sort.Slice(stamps, func(i, j int) bool { return stamps[i].Before(stamps[j]) })
	minGap := time.Hour
	for i := 1; i < len(stamps); i++ {
		if gap := stamps[i].Sub(stamps[i-1]); gap < minGap {
			minGap = gap
		}
	}
	// 8 independent nodes each spacing 60ms still interleave: aggregate gaps
	// collapse far below the Crawl-delay.
	if minGap >= crawlDelay/2 {
		t.Fatalf("independent locals held min gap %s >= %s — expected the N× overshoot", minGap, crawlDelay/2)
	}
	t.Logf("independent locals: min aggregate gap %s vs Crawl-delay %s (%.1f× overshoot)",
		minGap, crawlDelay, float64(crawlDelay)/float64(minGap))
}

// ReportCrawlDelay propagates to nodes that never saw robots.txt: a node
// passing minGap=0 still gets paced at the learned delay.
func TestReportCrawlDelayAppliesToUninformedNodes(t *testing.T) {
	shared := NewSimShared()
	shared.ReportCrawlDelay("example.com", 80*time.Millisecond)

	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := shared.Wait(context.Background(), "example.com", 0); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 2*80*time.Millisecond-30*time.Millisecond {
		t.Fatalf("learned Crawl-delay not enforced: 3 requests took %s, want >= ~160ms", elapsed)
	}
}

// A smaller reported delay never relaxes a larger one: the fleet keeps the
// strictest known Crawl-delay per host.
func TestReportCrawlDelayKeepsStrictest(t *testing.T) {
	shared := NewSimShared()
	shared.ReportCrawlDelay("example.com", 100*time.Millisecond)
	shared.ReportCrawlDelay("example.com", 20*time.Millisecond) // stale/lenient report

	start := time.Now()
	for i := 0; i < 2; i++ {
		if err := shared.Wait(context.Background(), "example.com", 0); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond-25*time.Millisecond {
		t.Fatalf("strictest delay lost: 2 requests took %s, want >= ~100ms", elapsed)
	}
}

// A canceled context aborts the wait instead of hanging the worker.
func TestWaitHonorsContextCancel(t *testing.T) {
	shared := NewSimShared()
	ctx := context.Background()
	if err := shared.Wait(ctx, "example.com", time.Hour); err != nil {
		t.Fatal(err) // first acquisition passes immediately
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := shared.Wait(ctx, "example.com", time.Hour); err == nil {
		t.Fatal("want context error, got nil")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("canceled wait took %s", elapsed)
	}
}
