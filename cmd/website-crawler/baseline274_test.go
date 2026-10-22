package main

// Phase 1 of #274 (crawler optimization + open-source crawler comparison):
// hermetic baseline profile of the crawler-node fetch pipeline.
//
// The full production crawlOne() cannot run against a loopback fixture by
// design — crawlOne refuses any target whose IP is not public (netguard
// security boundary; the test-only allowNonPublic hook only opens the dial
// path, not the PublicIP verdict). So the driver replicates crawlOne's exact
// sequencing (hostThrottle → robotsFor → [faithful: ipThrottle + sitePace] →
// fetchWebsiteInfo) through the production functions. Everything that touches
// the network, TLS, robots parsing, HTML parsing and politeness gating is the
// production code path; skipped are the DB layer (target selection, flush),
// the ASN breaker, and sitemap discovery.
//
// Run (external fixture, honest client-only profile):
//   go build -o /tmp/fixture-server ./tools/bench-fixture/cmd/fixture-server
//   go test -run 'TestBaseline274$' -v -timeout 30m \
//     -cpuprofile=/tmp/274-cpu.prof -memprofile=/tmp/274-mem.prof \
//     ./cmd/website-crawler/ -args -fixture-bin=/tmp/fixture-server
// Then: go tool pprof -top -nodecount=20 /tmp/274-cpu.prof
//
// Without -fixture-bin the fixture runs in-process (simpler, but the server's
// TLS/HTTP work then pollutes the crawler's profile).

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"codeberg.org/cl0qsearch/crawler-node/internal/config"
	"codeberg.org/cl0qsearch/crawler-node/netguard"
	"codeberg.org/cl0qsearch/crawler-node/tools/bench-fixture"
)

const (
	bench274Sites    = 1000
	bench274Iters    = 3
	bench274Workers  = 16 // WORKERS production default (validateConfig)
	bench274Latency  = 2 * time.Millisecond
	bench274TestName = "TestBaseline274"
)

// fixtureBin, when set, points at a built fixture-server binary; the fixture
// then runs as a separate OS process so the crawler's CPU profile is not
// polluted by the server's TLS/HTTP work. Build it with:
//
//	go build -o /tmp/fixture-server ./tools/bench-fixture/cmd/fixture-server
//
// and run the test with: go test -args -fixture-bin=/tmp/fixture-server
var fixtureBin = flag.String("fixture-bin", "", "path to fixture-server binary (external fixture process)")

// bench274Fixture abstracts the fixture server (in-process or external).
type bench274Fixture interface {
	Port() string
	Close()
}

// extFixture runs fixture-server as a child process.
type extFixture struct {
	cmd  *exec.Cmd
	port string
}

func (e *extFixture) Port() string { return e.port }
func (e *extFixture) Close() {
	if e.cmd != nil && e.cmd.Process != nil {
		_ = e.cmd.Process.Kill()
		_ = e.cmd.Wait()
	}
}

func startExtFixture(t *testing.T, n int) bench274Fixture {
	t.Helper()
	cmd := exec.Command(*fixtureBin, "-n", fmt.Sprint(n), "-latency", bench274Latency.String())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fixture-server: %v", err)
	}
	port := ""
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), "PORT="); ok {
			port = strings.TrimSpace(p)
			break
		}
	}
	if port == "" {
		_ = cmd.Process.Kill()
		t.Fatal("fixture-server did not print PORT=")
	}
	f := &extFixture{cmd: cmd, port: port}
	t.Cleanup(f.Close)
	return f
}
func bench274IP(i int) string {
	return fmt.Sprintf("127.1.%d.%d", 1+(i/250), 1+(i%250))
}

// bench274Setup installs the test-only hooks needed to crawl a loopback
// fixture through the production transport. Callers must run the returned
// cleanup func.
func bench274Setup(t *testing.T, n int) bench274Fixture {
	t.Helper()

	// 1. netguard dial guard: allow loopback (test-only hook; the PublicIP
	//    verdict used by crawlOne stays closed — see file header).
	oldAllow := netguard.GetAllowNonPublicForTest()
	netguard.SetAllowNonPublicForTest(true)

	// 2. Fixture cert is for 127.0.0.1, not bench*.test: skip verification
	//    (test-only; the TLS handshake itself is still fully exercised).
	oldInsecure := transport.TLSClientConfig.InsecureSkipVerify
	transport.TLSClientConfig.InsecureSkipVerify = true

	// 3. Production politeness tunables (floors: 1s/site, 1s/IP).
	loadPoliteness()

	// 3b. Runtime state main() initializes before crawling (package vars are
	//     zero in the test binary).
	dialSem = make(chan struct{}, clampInt(envInt("MAX_CONNS", 48), 1, 256))
	ua, err := config.PublicUserAgentFor("bench274-node")
	if err != nil {
		t.Fatal(err)
	}
	userAgent = ua
	fetchTimeout = time.Duration(clampInt(envInt("FETCH_TIMEOUT_MS", 20000), 2000, 60000)) * time.Millisecond

	// 4. DNS stub: bench{i}.test -> distinct 127/8 address. Long TTL so the
	//    cache stays warm like production's 60s window over a fast batch.
	ips := make(map[string]net.IPAddr, n)
	for i := 0; i < n; i++ {
		ips[fixture.Host(i)] = net.IPAddr{IP: net.ParseIP(bench274IP(i))}
	}
	lookup := func(ctx context.Context, host string) ([]net.IPAddr, error) {
		if a, ok := ips[host]; ok {
			return []net.IPAddr{a}, nil
		}
		return nil, fmt.Errorf("bench274: no fixture address for %s", host)
	}
	withTestDNS(t, newDNSCache(time.Hour, lookup))

	var fx bench274Fixture
	if *fixtureBin != "" {
		fx = startExtFixture(t, n)
	} else {
		srv, err := fixture.Start(n, bench274Latency)
		if err != nil {
			t.Fatal(err)
		}
		fx = srv
	}

	t.Cleanup(func() {
		fx.Close()
		transport.TLSClientConfig.InsecureSkipVerify = oldInsecure
		netguard.SetAllowNonPublicForTest(oldAllow)
	})
	t.Logf("politeness: hostDelay=%s ipDelay=%s maxConns(dialSem)=%d fetchTimeout=%s",
		tunables.HostDelay(), tunables.IPDelay(), cap(dialSem), fetchTimeout)
	return fx
}

// bench274Pace runs the crawlOne pacing prologue: per-site spacing (which also
// paces the robots.txt GET), then per-server-IP spacing.
func bench274Pace(domain, ip string) {
	hostThrottle(domain)
	ipThrottle(ip)
}

// bench274CrawlSite runs one site through the crawlOne-equivalent pipeline:
// hostThrottle, then robots.txt first via robotsFor (which paces the robots
// GET on the IP gate), then the page fetch (https GET, body read, charset
// transcode, html.Parse, parseHTMLInfo incl. SimHash, challenge detection).
// This mirrors crawlOne's exact gate sequencing; pre-stamping the IP gate
// before robotsFor would double-throttle (a second full 1s wait).
//
// When faithful is true, the post-robots pacing production applies between
// the robots.txt GET and the page GET is included: per the politeness floors
// those two requests to one site/IP stay >=1s apart, so a fresh site costs
// ~1s of pacing. When false, the gates stay armed (same code path) but each
// (site, IP) pair is only touched once per iteration, so the floors don't
// bind and the number measures the fetch-pipeline CPU ceiling instead.
// Excluded in both modes: DB layer, ASN breaker, sitemap discovery.
func bench274CrawlSite(domain, ip string, faithful bool) (*WebsiteInfo, time.Duration, error) {
	start := time.Now()
	// crawlOne order: site gate first, then robotsFor (which stamps the IP
	// gate for the robots.txt GET). Pre-stamping the IP gate here as well
	// would double-throttle: robotsFor's ipThrottle would see our stamp and
	// sleep a full second on every fresh pair.
	hostThrottle(domain)
	ri := robotsFor(domain, ip)
	if ri.blockErr != nil {
		return nil, time.Since(start), ri.blockErr
	}
	if !ri.allowed {
		return nil, time.Since(start), errRobotsDisallowed
	}
	if faithful {
		ipThrottle(ip)             // space the page GET from the robots GET (IP gate)
		sitePace(domain, ri.delay) // ... and on the site gate (Crawl-delay or the 1s floor)
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	info, err := fetchWebsiteInfo(ctx, domain, ip, "", "", ri.rules)
	return info, time.Since(start), err
}

type bench274IterStats struct {
	elapsed  time.Duration
	pagesSec float64
	heapMB   float64 // HeapAlloc after iteration
	totalGB  float64 // TotalAlloc after iteration
	numGC    uint32
	siteMs   float64 // mean per-site worker wall time (pacing + robots + page)
}

// bench274RunIter crawls all sites once with faithful pacing as given and
// returns the iteration stats. Every site must come back HTTP 200 with a
// title and words, or the run fails (fixture breakage must not silently
// become a "fast" number).
func bench274RunIter(t *testing.T, port string, faithful bool) bench274IterStats {
	t.Helper()
	// Fresh-site semantics per iteration (like a production batch of new
	// hosts): drop robots verdicts and politeness gates. DNS stays warm.
	robotsCache.Range(func(k, v any) bool { robotsCache.Delete(k); return true })
	hostGates = sync.Map{}
	ipGates = sync.Map{}

	work := make(chan int, bench274Sites)
	for i := 0; i < bench274Sites; i++ {
		work <- i
	}
	close(work)

	var mu sync.Mutex
	var firstErr error
	var okCount int
	var sumSite time.Duration
	var sumFetch time.Duration

	t0 := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < bench274Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				domain := fmt.Sprintf("%s:%s", fixture.Host(i), port)
				info, dt, err := bench274CrawlSite(domain, bench274IP(i), faithful)
				mu.Lock()
				sumSite += dt
				sumFetch += dt
				if err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("site %d: %w", i, err)
					}
				} else if info.StatusCode != 200 || info.Title == "" || info.WordCount == 0 {
					if firstErr == nil {
						firstErr = fmt.Errorf("site %d: bad info status=%d title=%q words=%d",
							i, info.StatusCode, info.Title, info.WordCount)
					}
				} else {
					okCount++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	el := time.Since(t0)
	if firstErr != nil {
		t.Fatalf("faithful=%v: %v (ok=%d/%d)", faithful, firstErr, okCount, bench274Sites)
	}
	if okCount != bench274Sites {
		t.Fatalf("faithful=%v: only %d/%d sites ok", faithful, okCount, bench274Sites)
	}
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	return bench274IterStats{
		elapsed:  el,
		pagesSec: float64(bench274Sites) / el.Seconds(),
		heapMB:   float64(ms.HeapAlloc) / 1e6,
		totalGB:  float64(ms.TotalAlloc) / 1e9,
		numGC:    ms.NumGC,
		// mean per-site worker wall time (pacing + robots + page)
		siteMs: float64(sumSite.Microseconds()) / float64(bench274Sites) / 1000,
	}
}

// bench274Summarize logs per-iteration stats plus mean/p50 and returns them.
func bench274Summarize(t *testing.T, label string, stats []bench274IterStats) (mean, p50 float64) {
	t.Helper()
	rates := make([]float64, len(stats))
	var sum float64
	for i, st := range stats {
		rates[i] = st.pagesSec
		sum += st.pagesSec
		t.Logf("%s iter %d: %d pages in %s = %.1f pages/s (mean site wall %.1fms, heap %.0fMB, totalAlloc %.2fGB, gc %d)",
			label, i+1, bench274Sites, st.elapsed.Round(time.Millisecond), st.pagesSec,
			st.siteMs, st.heapMB, st.totalGB, st.numGC)
	}
	for i := 1; i < len(rates); i++ { // insertion sort; 3 elements
		for j := i; j > 0 && rates[j] < rates[j-1]; j-- {
			rates[j], rates[j-1] = rates[j-1], rates[j]
		}
	}
	mean, p50 = sum/float64(len(rates)), rates[len(rates)/2]
	t.Logf("%s BASELINE: mean %.1f pages/s, p50 %.1f pages/s over %d iterations x %d sites (%d workers)",
		label, mean, p50, bench274Iters, bench274Sites, bench274Workers)
	return mean, p50
}

// TestBaseline274 is the fetch-pipeline CPU-ceiling baseline: politeness
// gates are armed at the production floors but each (site, IP) pair is only
// touched once per iteration, so the floors don't bind. Run with -cpuprofile
// to capture where CPU goes per page.
func TestBaseline274(t *testing.T) {
	if testing.Short() {
		t.Skip("baseline benchmark needs -short=false")
	}
	srv := bench274Setup(t, bench274Sites)
	port := srv.Port()

	stats := make([]bench274IterStats, 0, bench274Iters)
	for iter := 0; iter < bench274Iters; iter++ {
		stats = append(stats, bench274RunIter(t, port, false))
	}
	bench274Summarize(t, "pipeline-ceiling", stats)
}

// TestBaseline274Faithful is the production-faithful throughput baseline: the
// same pipeline as TestBaseline274 plus the post-robots pacing crawlOne
// applies (page GET >=1s after the robots.txt GET per site/IP). This is the
// number that binds in production; expect ~workers/1s.
func TestBaseline274Faithful(t *testing.T) {
	if testing.Short() {
		t.Skip("baseline benchmark needs -short=false")
	}
	srv := bench274Setup(t, bench274Sites)
	port := srv.Port()

	stats := make([]bench274IterStats, 0, bench274Iters)
	for iter := 0; iter < bench274Iters; iter++ {
		stats = append(stats, bench274RunIter(t, port, true))
	}
	bench274Summarize(t, "production-faithful", stats)
}

// TestBaseline274PolitenessFloor verifies the hard politeness floors with a
// single site hit 5 times in a row: requests must stay >=1s apart
// (minHostDelay/minIPDelay). Not part of the CPU profile run.
func TestBaseline274PolitenessFloor(t *testing.T) {
	if testing.Short() {
		t.Skip("needs -short=false")
	}
	srv := bench274Setup(t, 1)
	port := srv.Port()
	domain := fmt.Sprintf("%s:%s", fixture.Host(0), port)
	ip := bench274IP(0)
	const hits = 5
	starts := make([]time.Time, 0, hits)
	for k := 0; k < hits; k++ {
		bench274Pace(domain, ip)
		starts = append(starts, time.Now()) // post-pacing: the fetch start
		if _, _, err := bench274CrawlSite(domain, ip, false); err != nil {
			t.Fatalf("hit %d: %v", k, err)
		}
	}
	for k := 1; k < hits; k++ {
		gap := starts[k].Sub(starts[k-1])
		t.Logf("hit %d gap: %s", k, gap.Round(time.Millisecond))
		if gap < 950*time.Millisecond {
			t.Fatalf("hit %d gap %s < 1s politeness floor", k, gap)
		}
	}
}
