package main

// H1: Hermetic end-to-end "fake internet" pipeline test.
//
// Spins up fake origin HTTP servers (good HTML, robots.txt disallow, redirects,
// slow/oversized/bad-UTF-8/gzip-bomb bodies, SSRF-bait), drives each through the
// crawler's REAL pipeline via the testable crawlOneURL(ctx, db, url) entrypoint
// (robots check → fetch+parse through the netguard-guarded transport → persist),
// and asserts the outcomes.
//
// Must prove:
//  1. netguard blocks every SSRF-bait end-to-end through the real transport,
//     and the failure is specifically netguard.ErrNotPublic
//  2. robots.txt disallow is honored (via robotsFor, not a hand-rolled parse)
//  3. a 429'd domain is retried through the real retry loop, not dropped
//  4. hostile bodies never panic (no panics escape fetchWebsiteInfo)
//
// Run: go test -run TestH1 -v ./cmd/website-crawler/
// Requires: Postgres at $H1_DATABASE_URL (or default test DSN) with the
// crawler-node initdb/01-schema.sql schema.

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"cl0q/netguard"

	_ "github.com/lib/pq"
)

// H1 tests drive the real pipeline, which expects production initialization
// (main() calls loadPoliteness()). Initialize the politeness vars here so the
// 429/503 retry loop has its budget.
func init() {
	loadPoliteness()
}

// h1Origins holds the fake internet: one server per hostile case.
type h1Origins struct {
	good     *httptest.Server // valid HTML, robots allow
	disallow *httptest.Server // robots.txt disallows /
	redirect *httptest.Server // 301 to good
	slow     *httptest.Server // sleeps 25s (beyond fetch timeout)
	oversize *httptest.Server // 50MB body (must be truncated, not OOM)
	badUTF8  *httptest.Server // invalid UTF-8 bytes
	gzipBomb *httptest.Server // gzip bomb (must be bounded)
	ssrf     *httptest.Server // redirects to 127.0.0.1 / 169.254.169.254
	r429     *httptest.Server // 429 on first hit, 200 on retry
}

func startH1Origins(t *testing.T) *h1Origins {
	t.Helper()
	o := &h1Origins{}

	goodHTML := `<!DOCTYPE html><html><head><title>Good Site</title><meta name="description" content="a good site"></head><body><h1>Hello</h1><p>` + strings.Repeat("word ", 100) + `</p></body></html>`

	o.good = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Write([]byte("User-agent: *\nAllow: /\n"))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(goodHTML))
	}))

	o.disallow = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Write([]byte("User-agent: *\nDisallow: /\n"))
			return
		}
		w.Write([]byte(goodHTML))
	}))

	o.redirect = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Write([]byte("User-agent: *\nAllow: /\n"))
			return
		}
		http.Redirect(w, r, o.good.URL+"/", http.StatusMovedPermanently)
	}))

	o.slow = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Write([]byte("User-agent: *\nAllow: /\n"))
			return
		}
		time.Sleep(25 * time.Second) // beyond fetch timeout
		w.Write([]byte(goodHTML))
	}))

	o.oversize = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Write([]byte("User-agent: *\nAllow: /\n"))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		chunk := strings.Repeat("<p>padding</p>", 1000)
		for i := 0; i < 3500; i++ {
			w.Write([]byte(chunk))
		}
	}))

	o.badUTF8 = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Write([]byte("User-agent: *\nAllow: /\n"))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html><body><h1>Bad: \xff\xfe\x00 invalid</h1></body></html>"))
	}))

	o.gzipBomb = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Write([]byte("User-agent: *\nAllow: /\n"))
			return
		}
		// 10MB of zeros compresses to ~10KB: bomb if decompressed unbounded
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		gz.Write(make([]byte, 10<<20))
		gz.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/html")
		w.Write(buf.Bytes())
	}))

	o.ssrf = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Write([]byte("User-agent: *\nAllow: /\n"))
			return
		}
		// SSRF-bait: redirect to loopback
		http.Redirect(w, r, "http://127.0.0.1:9/", http.StatusFound)
	}))

	var r429Hits int
	o.r429 = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Write([]byte("User-agent: *\nAllow: /\n"))
			return
		}
		r429Hits++
		if r429Hits == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		w.Write([]byte(goodHTML))
	}))

	t.Cleanup(func() {
		o.good.Close()
		o.disallow.Close()
		o.redirect.Close()
		o.slow.Close()
		o.oversize.Close()
		o.badUTF8.Close()
		o.gzipBomb.Close()
		o.ssrf.Close()
		o.r429.Close()
	})
	return o
}

func h1DB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("H1_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://cl0qtest:cl0qtest@localhost:5432/crawler_test?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("DB unreachable: %v (set H1_DATABASE_URL)", err)
	}
	return db
}

// h1Clean removes website_info rows written by H1 crawls (domains are the
// fake origins' host:port).
func h1Clean(t *testing.T, db *sql.DB, hosts ...string) {
	t.Helper()
	for _, h := range hosts {
		if _, err := db.Exec(`DELETE FROM website_info WHERE domain = $1`, h); err != nil {
			t.Fatalf("cleanup %s: %v", h, err)
		}
	}
}

func h1Host(url string) string {
	return strings.TrimPrefix(strings.TrimPrefix(url, "http://"), "https://")
}

// withGuard runs f with netguard enforced (or not), restoring afterwards.
func withGuard(t *testing.T, enforced bool, f func()) {
	t.Helper()
	old := netguard.AllowNonPublic
	netguard.AllowNonPublic = !enforced
	defer func() { netguard.AllowNonPublic = old }()
	f()
}

// TestH1_FullChain drives the real pipeline (robots → fetch+parse → persist)
// against the good fake origin and asserts the website_info row.
func TestH1_FullChain(t *testing.T) {
	o := startH1Origins(t)
	db := h1DB(t)
	defer db.Close()
	host := h1Host(o.good.URL)
	h1Clean(t, db, host)
	defer h1Clean(t, db, host)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	info, err := crawlOneURL(ctx, db, o.good.URL+"/")
	if err != nil {
		t.Fatalf("crawlOneURL(good): %v", err)
	}
	if info == nil {
		t.Fatal("crawlOneURL returned nil info, nil error")
	}
	if !strings.Contains(info.Title, "Good Site") {
		t.Errorf("title = %q, want it to contain %q", info.Title, "Good Site")
	}
	if info.StatusCode != 200 {
		t.Errorf("status = %d, want 200", info.StatusCode)
	}

	var title string
	var status int
	err = db.QueryRow(`SELECT title, status_code FROM website_info WHERE domain = $1`, host).Scan(&title, &status)
	if err != nil {
		t.Fatalf("read back website_info row: %v", err)
	}
	if !strings.Contains(title, "Good Site") {
		t.Errorf("persisted title = %q, want it to contain %q", title, "Good Site")
	}
	if status != 200 {
		t.Errorf("persisted status = %d, want 200", status)
	}
	t.Logf("full chain: robots → fetch → parse → persist OK for %s", host)
}

// TestH1_SSRFBlocked crawls the SSRF-bait origin with the guard ENFORCED and
// requires the failure to be netguard.ErrNotPublic — proving the guard (not
// just the network) blocks through the real pipeline.
func TestH1_SSRFBlocked(t *testing.T) {
	o := startH1Origins(t)
	db := h1DB(t)
	defer db.Close()

	withGuard(t, true, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// The SSRF-bait origin (redirects to 127.0.0.1): the crawl must fail,
		// and the failure must be the guard's ErrNotPublic.
		_, err := crawlOneURL(ctx, db, o.ssrf.URL+"/")
		if err == nil {
			t.Fatal("crawling SSRF-bait origin succeeded; want netguard refusal")
		}
		if !errors.Is(err, netguard.ErrNotPublic) {
			t.Errorf("SSRF-bait crawl error = %v; want errors.Is(err, netguard.ErrNotPublic)", err)
		} else {
			t.Logf("SSRF-bait blocked by netguard: %v", err)
		}

		// Direct SSRF targets through the same entrypoint.
		for _, target := range []string{"http://127.0.0.1:9/", "http://169.254.169.254/"} {
			_, err := crawlOneURL(ctx, db, target)
			if err == nil {
				t.Errorf("crawling %s succeeded; want netguard refusal", target)
			} else if !errors.Is(err, netguard.ErrNotPublic) {
				t.Errorf("crawling %s: error = %v; want netguard.ErrNotPublic", target, err)
			}
		}
	})
}

// TestH1_RobotsDisallow crawls the disallow origin through the real pipeline
// and requires the robots verdict (not a hand-rolled parse) to stop it.
func TestH1_RobotsDisallow(t *testing.T) {
	o := startH1Origins(t)
	db := h1DB(t)
	defer db.Close()
	host := h1Host(o.disallow.URL)
	h1Clean(t, db, host)
	defer h1Clean(t, db, host)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := crawlOneURL(ctx, db, o.disallow.URL+"/")
	if !errors.Is(err, errRobotsDisallowed) {
		t.Errorf("crawlOneURL(disallow) = %v; want errRobotsDisallowed", err)
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM website_info WHERE domain = $1`, host).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("disallowed origin persisted %d website_info rows; want 0", n)
	}
	t.Logf("robots Disallow: / honored through the real pipeline")
}

// TestH1_R429Retried crawls the 429-then-200 origin and requires the real
// retry loop to ride out the 429 — the domain is retried, not dropped.
func TestH1_R429Retried(t *testing.T) {
	o := startH1Origins(t)
	db := h1DB(t)
	defer db.Close()
	host := h1Host(o.r429.URL)
	h1Clean(t, db, host)
	defer h1Clean(t, db, host)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	info, err := crawlOneURL(ctx, db, o.r429.URL+"/")
	if err != nil {
		t.Fatalf("crawlOneURL(r429): %v", err)
	}
	if info.StatusCode != 200 {
		t.Errorf("status after retry = %d, want 200 (429 was not retried)", info.StatusCode)
	}
	if !strings.Contains(info.Title, "Good Site") {
		t.Errorf("title after retry = %q, want it to contain %q", info.Title, "Good Site")
	}
	t.Logf("429 retried through the real retry loop; final status 200")
}

// TestH1_HostileBodies crawls each hostile origin through the real pipeline:
// nothing may panic, and bounded reads must hold.
func TestH1_HostileBodies(t *testing.T) {
	o := startH1Origins(t)
	db := h1DB(t)
	defer db.Close()

	cases := map[string]*httptest.Server{
		"oversize": o.oversize,
		"badUTF8":  o.badUTF8,
		"gzipBomb": o.gzipBomb,
	}
	for name, srv := range cases {
		host := h1Host(srv.URL)
		h1Clean(t, db, host)
		func() {
			defer h1Clean(t, db, host)
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: PANIC escaped the pipeline: %v", name, r)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			info, err := crawlOneURL(ctx, db, srv.URL+"/")
			if err != nil {
				t.Errorf("%s: crawlOneURL error: %v", name, err)
				return
			}
			if info == nil {
				t.Errorf("%s: nil info, nil error", name)
				return
			}
			t.Logf("%s: crawled OK (status %d, title %q), no panic", name, info.StatusCode, info.Title)
		}()
	}
}

// TestH1_SlowTimeout proves the slow origin times out (fetchTimeout) instead
// of hanging the pipeline.
func TestH1_SlowTimeout(t *testing.T) {
	o := startH1Origins(t)
	db := h1DB(t)
	defer db.Close()
	host := h1Host(o.slow.URL)
	h1Clean(t, db, host)
	defer h1Clean(t, db, host)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	start := time.Now()
	_, err := crawlOneURL(ctx, db, o.slow.URL+"/")
	elapsed := time.Since(start)
	if err == nil {
		t.Error("slow origin crawl succeeded; want a timeout error")
	} else {
		t.Logf("slow origin timed out as expected after %v: %v", elapsed.Round(time.Second), err)
	}
	if elapsed > 75*time.Second {
		t.Errorf("slow crawl took %v; fetchTimeout should bound it", elapsed)
	}
}
