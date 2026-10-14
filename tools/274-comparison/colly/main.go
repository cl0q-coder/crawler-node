// Command colly274 runs the #274 phase-3 head-to-head for Colly (gocolly/colly).
//
// It replicates the production-faithful semantics of crawler-node's baseline
// (docs/274-baseline.md): 16 workers; each worker takes a host, fetches
// robots.txt, waits out the 1s politeness floor, fetches the homepage, then
// moves to the next host. The two-phase visit is scripted explicitly;
// colly's built-in robots.txt handling is disabled (IgnoreRobotsTxt) so the
// scripted robots fetch is the only one and the timed region measures
// exactly the 2000 scripted requests.
//
// Why sticky workers instead of colly's LimitRule or a global semaphore:
//   - LimitRule matches against request.URL.Host which INCLUDES the port, so
//     a naive "bench*.test" glob silently matches nothing (caught 2026-10-13:
//     harness ran unthrottled at 350 pages/s).
//   - The rule's waitChan is per-RULE not per-domain, so one rule with
//     Parallelism=1 serializes the ENTIRE crawl globally (caught 2026-10-13:
//     999 goroutines wedged, ~0 pages in 7 minutes).
//   - A global 16-slot semaphore + per-host gate lets all robots jump ahead
//     of pages (FIFO starvation): the 1s gap is honored but scheduling
//     differs from crawler-node's worker model, so throughput is not
//     comparable (caught 2026-10-13: 298 pages/s with gaps honored).
// Sticky workers replicate the crawler-node faithful run exactly: each
// worker is occupied for the full robots→1s→page sequence per host.
//
// TLS verification is skipped (test-only): the fixture uses a throwaway
// self-signed CA, same as the baseline harness's InsecureSkipVerify. The TLS
// handshake itself (crypto) is fully exercised.
//
// Usage: PORT=44839 colly274   (fixture must be running; bench{i}.test must
// resolve, e.g. via the /tmp/hosts.new bind-mount used in phase 3)
package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gocolly/colly/v2"
)

const (
	nSites   = 1000
	parallel = 16 // workers, like WORKERS=16 in the baseline
	hostGap  = time.Second
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		fmt.Fprintln(os.Stderr, "PORT env required")
		os.Exit(1)
	}

	newCollector := func() *colly.Collector {
		// Per-worker dial: keep it simple, resolve via system resolver.
		dialer := &net.Dialer{Timeout: 30 * time.Second}
		tr := &http.Transport{
			DialContext:           dialer.DialContext,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: true}, // test-only, mirrors baseline harness
			MaxIdleConnsPerHost:   2,
			ResponseHeaderTimeout: 30 * time.Second,
		}
		c := colly.NewCollector() // synchronous: one worker drives it
		c.WithTransport(tr)
		// Robots are fetched explicitly (two-phase script); colly's built-in
		// checkRobots would otherwise issue a second, uncounted robots fetch
		// per host inside the timed region.
		c.IgnoreRobotsTxt = true
		return c
	}

	var mu sync.Mutex
	lastReq := map[string]time.Time{}
	minGap := time.Hour
	var pages, robots, errs int
	var bytes int64

	// attach shared counters + per-host gap measurement to a collector
	wire := func(c *colly.Collector) {
		c.OnRequest(func(r *colly.Request) {
			host := r.URL.Hostname()
			mu.Lock()
			now := time.Now()
			if prev, ok := lastReq[host]; ok {
				if g := now.Sub(prev); g < minGap {
					minGap = g
				}
			}
			lastReq[host] = now
			mu.Unlock()
		})
		c.OnResponse(func(r *colly.Response) {
			mu.Lock()
			defer mu.Unlock()
			if strings.HasSuffix(r.Request.URL.Path, "/robots.txt") {
				if r.StatusCode == 200 {
					robots++
				} else {
					errs++
				}
				return
			}
			if r.StatusCode == 200 && strings.Contains(string(r.Body), "<title>") {
				pages++
				bytes += int64(len(r.Body))
			} else {
				errs++
			}
		})
		c.OnError(func(r *colly.Response, err error) {
			mu.Lock()
			errs++
			mu.Unlock()
			fmt.Fprintf(os.Stderr, "ERR %s: %v\n", r.Request.URL, err)
		})
	}

	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < parallel; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			c := newCollector()
			wire(c)
			for i := w; i < nSites; i += parallel {
				host := fmt.Sprintf("bench%d.test", i)
				t0 := time.Now()
				if err := c.Visit(fmt.Sprintf("https://%s:%s/robots.txt", host, port)); err != nil {
					mu.Lock()
					errs++
					mu.Unlock()
				}
				// Politeness floor: page starts >=1s after robots started,
				// exactly like the faithful crawler-node run.
				if d := time.Until(t0.Add(hostGap)); d > 0 {
					time.Sleep(d)
				}
				if err := c.Visit(fmt.Sprintf("https://%s:%s/", host, port)); err != nil {
					mu.Lock()
					errs++
					mu.Unlock()
				}
			}
		}(w)
	}
	wg.Wait()
	wall := time.Since(start)

	mu.Lock()
	defer mu.Unlock()
	fmt.Printf("colly: pages=%d robots=%d errs=%d bytes=%d wall=%.3fs pages_per_s=%.2f min_per_host_gap_ms=%.1f\n",
		pages, robots, errs, bytes, wall.Seconds(),
		float64(pages)/wall.Seconds(), float64(minGap)/float64(time.Millisecond))
}
