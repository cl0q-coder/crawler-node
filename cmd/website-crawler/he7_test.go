package main

// HE-6/HE-7 reconciled tests: half-open breaker canary + robots.txt TTL cache.
// The canary semantics are the reconciled ones: the breaker owns WHEN
// (tryProbe), the frontier reserve owns WHICH domain (t.probe), success
// closes early, failure does NOT ratchet the cooldown.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testBreaker returns a breaker with tiny timings for fast tests.
func testBreaker() *asnBreaker {
	return &asnBreaker{enabled: true, threshold: 1, window: time.Hour, base: 50 * time.Millisecond, cap: time.Hour}
}

func breakStateOf(b *asnBreaker, key string) *breakState {
	v, _ := b.m.Load(key)
	return v.(*breakState)
}

// TestTryProbeAdmitsExactlyOneCanary: while a group is open, the first
// tryProbe admits, the second is denied (one in flight).
func TestTryProbeAdmitsExactlyOneCanary(t *testing.T) {
	b := testBreaker()
	key := "he7-one"
	b.fail(key) // trips: open ~50ms
	if !b.blocked(key) {
		t.Fatal("group should be open after trip")
	}
	if !b.tryProbe(key) {
		t.Fatal("first tryProbe while open must admit the canary")
	}
	if b.tryProbe(key) {
		t.Fatal("second tryProbe must be denied while a probe is in flight")
	}
}

// TestTryProbeDeniedWhenNotOpen: no state / closed group → no probe.
func TestTryProbeDeniedWhenNotOpen(t *testing.T) {
	b := testBreaker()
	if b.tryProbe("he7-never-seen") {
		t.Fatal("tryProbe on an unknown group must be denied")
	}
	b.fail("he7-closed")
	s := breakStateOf(b, "he7-closed")
	s.mu.Lock()
	s.openUntil = time.Now().Add(-time.Second) // lapse the cooldown
	s.mu.Unlock()
	if b.tryProbe("he7-closed") {
		t.Fatal("tryProbe on a closed group must be denied (normal path handles it)")
	}
}

// TestProbeSuccessClosesEarly: a successful canary closes the breaker
// immediately instead of waiting out the cooldown — the fast recovery that
// keeps the frontier moving.
func TestProbeSuccessClosesEarly(t *testing.T) {
	b := testBreaker()
	key := "he7-early"
	b.fail(key)
	if !b.tryProbe(key) {
		t.Fatal("canary not admitted")
	}
	b.success(key) // the probe's fetch got a real response
	if b.blocked(key) {
		t.Fatal("successful probe must close the breaker early, not wait out the cooldown")
	}
	s := breakStateOf(b, key)
	s.mu.Lock()
	level := s.level
	s.mu.Unlock()
	if level != 0 {
		t.Fatalf("level after probe success = %d, want 0", level)
	}
}

// TestProbeFailureDoesNotRatchet: a failed canary leaves the cooldown
// UNCHANGED — no extension, no escalation. This is the HE-6/HE-7 reconcile:
// #187 re-tripped with escalation here; #181 required probes to never
// ratchet the cooldown. Replaces TestProbeFailureReopensWithEscalation.
func TestProbeFailureDoesNotRatchet(t *testing.T) {
	b := &asnBreaker{enabled: true, threshold: 1, window: time.Hour, base: 40 * time.Millisecond, cap: time.Hour}
	key := "he7-sick"
	b.fail(key) // level 1, ~40ms
	s := breakStateOf(b, key)
	s.mu.Lock()
	firstUntil := s.openUntil
	level := s.level
	s.mu.Unlock()

	if !b.tryProbe(key) {
		t.Fatal("canary not admitted")
	}
	b.fail(key) // the canary timed out: still sick
	s.mu.Lock()
	secondUntil := s.openUntil
	levelAfter := s.level
	inFlight := s.probeInFlight
	s.mu.Unlock()

	if !secondUntil.Equal(firstUntil) {
		t.Fatalf("failed probe moved the cooldown: %v -> %v (ratchet!)", firstUntil, secondUntil)
	}
	if levelAfter != level {
		t.Fatalf("level after failed probe = %d, want %d (no escalation)", levelAfter, level)
	}
	if inFlight {
		t.Fatal("probeInFlight must be cleared after the probe reports")
	}
	if !b.blocked(key) {
		t.Fatal("group must still be open after a failed probe")
	}
}

// TestProbeIntervalSpacing: canaries are spaced — a group probed recently
// isn't probed again until the interval lapses.
func TestProbeIntervalSpacing(t *testing.T) {
	b := testBreaker()
	key := "he7-spacing"
	b.fail(key)
	if !b.tryProbe(key) {
		t.Fatal("canary not admitted")
	}
	b.success(key) // closed early
	b.fail(key)    // trips again immediately
	if b.tryProbe(key) {
		t.Fatal("second probe admitted too soon after the first (min 30s interval)")
	}
}

// TestProbeStuckDoesNotWedge: a probe that never reports (panic path) doesn't
// wedge the group forever — after two silent intervals a fresh probe is allowed.
func TestProbeStuckDoesNotWedge(t *testing.T) {
	b := testBreaker()
	key := "he7-stuck"
	b.fail(key)
	if !b.tryProbe(key) {
		t.Fatal("canary not admitted")
	}
	s := breakStateOf(b, key)
	s.mu.Lock()
	s.lastProbe = time.Now().Add(-10 * time.Minute) // simulate a long-dead probe
	// keep the group open so the probe path is what we're testing
	s.openUntil = time.Now().Add(time.Hour)
	s.mu.Unlock()
	if !b.tryProbe(key) {
		t.Fatal("stale in-flight probe wedged the group: fresh probe denied")
	}
}

// TestProbeConcurrentAdmission: 32 goroutines racing tryProbe admit exactly
// one canary.
func TestProbeConcurrentAdmission(t *testing.T) {
	b := testBreaker()
	key := "he7-race"
	b.fail(key)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.tryProbe(key) {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := admitted.Load(); n != 1 {
		t.Fatalf("%d canaries admitted under race, want exactly 1", n)
	}
}

// TestReconciledCanaryJoint: the HE-6/HE-7 reconciled semantics, end to end
// at the unit level (Claude's JOINT TEST):
//  1. a breaker-open group gets exactly ONE probe per interval, and only a
//     frontier-reserve probe (t.probe) is eligible;
//  2. a probe SUCCESS closes the breaker early;
//  3. repeated probe FAILURES do NOT ratchet the cooldown;
//  4. the group is never double-probed.
func TestReconciledCanaryJoint(t *testing.T) {
	t.Setenv("FRONTIER_PROBE_PCT", "10")
	// admitCanary reads the global breaker: isolate it for the test.
	oldEnabled, oldThreshold := breaker.enabled, breaker.threshold
	oldWindow, oldBase, oldCap := breaker.window, breaker.base, breaker.cap
	breaker.enabled = true
	breaker.threshold = 1
	breaker.window = time.Hour
	breaker.base = time.Hour
	breaker.cap = 2 * time.Hour
	t.Cleanup(func() {
		breaker.enabled, breaker.threshold = oldEnabled, oldThreshold
		breaker.window, breaker.base, breaker.cap = oldWindow, oldBase, oldCap
	})

	key := "asn:JointASN"
	mk := func(d string, probe bool) target {
		return target{domain: d, org: "JointASN", probe: probe}
	}
	breaker.fail(key) // trip: open ~1h
	if !breaker.blocked(key) {
		t.Fatal("group should be open after trip")
	}

	// (1) only reserve-marked probes are eligible, exactly one per interval.
	if admitCanary(mk("j-plain.joint.example", false)) {
		t.Fatal("non-probe target admitted for open group: only reserve probes may canary")
	}
	if !admitCanary(mk("j-p1.joint.example", true)) {
		t.Fatal("first reserve probe denied")
	}
	if admitCanary(mk("j-p2.joint.example", true)) {
		t.Fatal("second probe admitted while one is in flight: group double-probed")
	}

	// (2) probe SUCCESS closes the breaker early.
	breaker.success(key)
	if breaker.blocked(key) {
		t.Fatal("successful probe did not close the breaker early")
	}

	// (3) repeated probe FAILURES do not ratchet the cooldown.
	breaker.fail(key) // re-trip
	s := breakStateOf(breaker, key)
	s.mu.Lock()
	openUntil, level := s.openUntil, s.level
	s.mu.Unlock()
	for i := 0; i < 3; i++ {
		if !admitCanary(mk(fmt.Sprintf("j-f%d.joint.example", i), true)) {
			// interval gate: age the last probe, simulating time passing
			s.mu.Lock()
			s.lastProbe = time.Now().Add(-10 * time.Minute)
			s.mu.Unlock()
			if !admitCanary(mk(fmt.Sprintf("j-f%d.joint.example", i), true)) {
				t.Fatalf("probe %d denied after aging lastProbe", i)
			}
		}
		breaker.fail(key) // the canary found the group still sick
		s.mu.Lock()
		u, l, inFlight := s.openUntil, s.level, s.probeInFlight
		s.mu.Unlock()
		if !u.Equal(openUntil) {
			t.Fatalf("probe failure %d moved the cooldown: %v -> %v (ratchet!)", i, openUntil, u)
		}
		if l != level {
			t.Fatalf("probe failure %d changed level: %d -> %d (ratchet!)", i, level, l)
		}
		if inFlight {
			t.Fatalf("probe failure %d left probeInFlight set", i)
		}
		if !breaker.blocked(key) {
			t.Fatalf("group closed after probe failure %d", i)
		}
	}
}

// --- robots.txt TTL ---

func TestCachedRobotsTTL(t *testing.T) {
	host := "he7-ttl.example"
	robotsCache = sync.Map{}
	robotsCache.Store(host, robotsInfo{allowed: true, fetchedAt: time.Now()})
	if _, ok := cachedRobots(host); !ok {
		t.Fatal("fresh entry must hit")
	}
	robotsCache.Store(host, robotsInfo{allowed: true, fetchedAt: time.Now().Add(-robotsTTL - time.Second)})
	if _, ok := cachedRobots(host); ok {
		t.Fatal("expired entry must miss")
	}
}

// TestPruneRobotsCache: expired entries are dropped, fresh ones kept.
func TestPruneRobotsCache(t *testing.T) {
	robotsCache = sync.Map{}
	robotsCache.Store("he7-fresh.example", robotsInfo{fetchedAt: time.Now()})
	robotsCache.Store("he7-stale.example", robotsInfo{fetchedAt: time.Now().Add(-2 * robotsTTL)})
	pruneRobotsCache()
	if _, ok := robotsCache.Load("he7-fresh.example"); !ok {
		t.Fatal("fresh entry pruned")
	}
	if _, ok := robotsCache.Load("he7-stale.example"); ok {
		t.Fatal("expired entry not pruned")
	}
}

// TestRobotsForFetchesOncePerTTL: the HE-7 behavior change — robots.txt is
// fetched once and reused across what used to be batch wipes, refetched only
// after the TTL lapses.
func TestRobotsForFetchesOncePerTTL(t *testing.T) {
	var mu sync.Mutex
	fetches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fetches++
		mu.Unlock()
		fmt.Fprint(w, "User-agent: *\nCrawl-delay: 3\n")
	}))
	t.Cleanup(srv.Close)
	host := srv.Listener.Addr().String()
	robotsCache = sync.Map{}
	t.Cleanup(func() { robotsCache = sync.Map{} })

	count := func() int { mu.Lock(); defer mu.Unlock(); return fetches }

	if ri := robotsFor(host, ""); !ri.allowed || ri.delay != 3*time.Second {
		t.Fatalf("first fetch: want allowed+3s delay, got %+v", ri)
	}
	for i := 0; i < 5; i++ {
		robotsFor(host, "") // what used to be separate batch cycles
	}
	if n := count(); n != 1 {
		t.Fatalf("robots.txt fetched %d times for 6 lookups, want 1 (TTL cache)", n)
	}
	// Age the entry past the TTL: the next lookup must refetch exactly once,
	// and concurrent refetchers still share one fetch (single-flight).
	robotsCache.Store(host, robotsInfo{allowed: true, fetchedAt: time.Now().Add(-robotsTTL - time.Second)})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); robotsFor(host, "") }()
	}
	wg.Wait()
	if n := count(); n != 2 {
		t.Fatalf("robots.txt fetched %d times total, want 2 (one refetch, single-flighted)", n)
	}
}

// --- benchmarks (Chris's standing requirement: real numbers) ---

func BenchmarkTryProbe(b *testing.B) {
	br := &asnBreaker{enabled: true, threshold: 1, window: time.Hour, base: time.Hour, cap: 2 * time.Hour}
	key := "he7-bench"
	br.fail(key) // open for ~1h so tryProbe takes the interval path
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		br.tryProbe(key)
	}
}

func BenchmarkCachedRobotsHit(b *testing.B) {
	robotsCache = sync.Map{}
	robotsCache.Store("he7-bench.example", robotsInfo{allowed: true, fetchedAt: time.Now()})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := cachedRobots("he7-bench.example"); !ok {
			b.Fatal("miss")
		}
	}
}

func BenchmarkPruneRobotsCache(b *testing.B) {
	robotsCache = sync.Map{}
	for i := 0; i < 10000; i++ {
		age := time.Duration(0)
		if i%2 == 0 {
			age = 2 * robotsTTL // half expired
		}
		robotsCache.Store(fmt.Sprintf("he7-%d.example", i), robotsInfo{fetchedAt: time.Now().Add(-age)})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pruneRobotsCache()
	}
}
