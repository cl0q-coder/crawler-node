// Command colly274 runs the #274 phase-3 head-to-head for Colly (gocolly/colly).
//
// It replicates the production-faithful semantics of crawler-node's baseline
// (docs/274-baseline.md): per fixture host, fetch robots.txt first, then the
// homepage, with a 1s minimum gap between requests to the same host and 16
// hosts in flight at once. Colly has no built-in robots.txt support, so the
// two-phase visit is scripted explicitly (robots URL first, homepage on
// completion) — the 1s LimitRule delay then binds exactly like the
// robots→page floor in the crawler-node faithful run.
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
	parallel = 16
	hostGap  = time.Second
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		fmt.Fprintln(os.Stderr, "PORT env required")
		os.Exit(1)
	}

	// Per-host dial: keep it simple, resolve via system resolver.
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true}, // test-only, mirrors baseline harness
		MaxIdleConnsPerHost:   2,
		ResponseHeaderTimeout: 30 * time.Second,
	}

	c := colly.NewCollector(colly.Async(true))
	c.WithTransport(tr)

	// 1s minimum gap between consecutive requests to the same host,
	// at most 1 in flight per host. Distinct hosts proceed in parallel.
	if err := c.Limit(&colly.LimitRule{
		DomainGlob:  "bench*.test",
		Delay:       hostGap,
		Parallelism: 1,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "limit:", err)
		os.Exit(1)
	}

	sem := make(chan struct{}, parallel) // global 16-way cap, like WORKERS=16

	var mu sync.Mutex
	lastReq := map[string]time.Time{}
	minGap := time.Hour
	var pages, robots, errs int
	var bytes int64

	c.OnRequest(func(r *colly.Request) {
		sem <- struct{}{}
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
	release := func() { <-sem }

	c.OnResponse(func(r *colly.Response) {
		defer release()
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.Request.URL.Path, "/robots.txt") {
			if r.StatusCode == 200 {
				robots++
			} else {
				errs++
			}
			// Phase 2: now visit the homepage; the 1s LimitRule delay
			// binds between robots and page, like the faithful run.
			pageURL := "https://" + r.Request.URL.Host + "/"
			if err := c.Visit(pageURL); err != nil {
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
		release()
		mu.Lock()
		errs++
		mu.Unlock()
		if r != nil && strings.HasSuffix(r.Request.URL.Path, "/robots.txt") {
			// robots failure: still try the page so the count stays honest
			_ = c.Visit("https://" + r.Request.URL.Host + "/")
		}
		fmt.Fprintf(os.Stderr, "ERR %s: %v\n", r.Request.URL, err)
	})

	start := time.Now()
	for i := 0; i < nSites; i++ {
		u := fmt.Sprintf("https://bench%d.test:%s/robots.txt", i, port)
		if err := c.Visit(u); err != nil {
			mu.Lock()
			errs++
			mu.Unlock()
		}
	}
	c.Wait()
	wall := time.Since(start)

	mu.Lock()
	defer mu.Unlock()
	fmt.Printf("colly: pages=%d robots=%d errs=%d bytes=%d wall=%.3fs pages_per_s=%.2f min_per_host_gap_ms=%.1f\n",
		pages, robots, errs, bytes, wall.Seconds(),
		float64(pages)/wall.Seconds(), float64(minGap)/float64(time.Millisecond))
}
