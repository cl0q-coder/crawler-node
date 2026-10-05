package main

// Politeness tests: how many connections a refusing / plain-http / closed server sees, pacing, and which robots.txt
// group applies. Fake servers on 127.0.0.1 count connections.  go test ./cmd/website-crawler

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"cl0q/botid"
	"cl0q/crawlutil"
	"cl0q/internal/tunable"
	"cl0q/netguard"
	"cl0q/robotstxt"
)

// connCounter accepts TCP connections and records when each arrived; handle decides what to do with it.
// main() normally creates the dial semaphore and the User-Agent; tests don't run main. The fake servers below listen
// on 127.0.0.1, which netguard refuses, so the guard is off here except in the tests that check it.
func init() {
	dialSem = make(chan struct{}, 8)
	userAgent, _ = botid.UserAgent("test-node")
	netguard.SetAllowNonPublicForTest(true)
}

type connCounter struct {
	ln    net.Listener
	mu    sync.Mutex
	times []time.Time
}

func newCounter(t *testing.T, handle func(net.Conn)) *connCounter {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &connCounter{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c.mu.Lock()
			c.times = append(c.times, time.Now())
			c.mu.Unlock()
			go handle(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return c
}

func (c *connCounter) n() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.times) }

func init() {
	tunables = tunable.New(time.Second, time.Second, 50)
}

func intPtr(i int) *int { return &i }

func TestHTTPSUnavailable(t *testing.T) {
	yes := []string{"dial tcp 1.2.3.4:443: connect: connection refused", "tls: failed to verify certificate: x509: certificate signed by unknown authority",
		"http: server gave HTTP response to HTTPS client", "tls: first record does not look like a TLS handshake"}
	no := []string{"EOF", "read tcp 1.2.3.4:443: read: connection reset by peer", "context deadline exceeded", "net/http: TLS handshake timeout", "http: server closed idle connection"}
	for _, s := range yes {
		if !crawlutil.HTTPSUnavailable(errors.New(s)) {
			t.Errorf("want fallback for %q", s)
		}
	}
	for _, s := range no {
		if crawlutil.HTTPSUnavailable(errors.New(s)) {
			t.Errorf("want NO fallback for %q", s)
		}
	}
	if !isBackoffError(errors.New(`Get "https://x/": EOF`)) {
		t.Error("EOF (nginx 444) must count as back-off")
	}
}

// A server that drops every connection (nginx 444 / WAF) must get exactly ONE robots request, no http:// retry.
func TestRobotsBlockedServerGetsOneRequest(t *testing.T) {
	tunables.Update(tunable.Config{IPDelayMs: intPtr(0)})
	c := newCounter(t, func(conn net.Conn) { conn.Close() })
	host := c.ln.Addr().String()
	ri := fetchRobots(host, "")
	time.Sleep(150 * time.Millisecond)
	if ri.blockErr == nil {
		t.Fatalf("want blockErr, got %+v", ri)
	}
	if got := c.n(); got != 1 {
		t.Fatalf("blocked server saw %d connections, want 1", got)
	}
	robotsCache = sync.Map{}
	if r2 := robotsFor(host, ""); r2.blockErr == nil {
		t.Fatal("robotsFor must surface the block")
	}
	if _, cached := robotsCache.Load(host); cached {
		t.Fatal("a blocked result must not be cached")
	}
}

// Plain-HTTP server: https fails as "not TLS" → ONE paced http:// fallback, and the gap respects ipDelay.
func TestFallbackIsPaced(t *testing.T) {
	tunables.Update(tunable.Config{IPDelayMs: intPtr(300)})
	ipGates = sync.Map{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("User-agent: *\nCrawl-delay: 2\n"))
	})}
	c := newCounter(t, func(conn net.Conn) {})
	c.ln.Close() // replace the counter's raw accept loop with a real HTTP server on a fresh listener
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	var mu sync.Mutex
	var seen []time.Time
	go srv.Serve(&countingListener{Listener: ln, onAccept: func() { mu.Lock(); seen = append(seen, time.Now()); mu.Unlock() }})
	t.Cleanup(func() { srv.Close() })
	host := ln.Addr().String()
	ipThrottle("10.9.9.9") // stamp the gate as if the robots GET's own pre-throttle just ran
	ri := fetchRobots(host, "10.9.9.9")
	if ri.blockErr != nil || !ri.allowed || ri.delay != 2*time.Second {
		t.Fatalf("want parsed robots (allowed, 2s), got %+v", ri)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("want 2 connections (failed https + http), got %d", len(seen))
	}
	if gap := seen[1].Sub(seen[0]); gap < 250*time.Millisecond {
		t.Fatalf("http fallback not paced: gap %v < %v", gap, tunables.IPDelay())
	}
}

// Closed port: https refused → http tried (also refused) → permissive, not a block.
func TestRefusedPortIsNotABlock(t *testing.T) {
	tunables.Update(tunable.Config{IPDelayMs: intPtr(0)})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	host := ln.Addr().String()
	ln.Close()
	ri := fetchRobots(host, "")
	if ri.blockErr != nil || !ri.allowed {
		t.Fatalf("refused port should be permissive, got %+v", ri)
	}
}

// The page fetch must also stop after a dropped connection instead of retrying over http://.
func TestPageFetchNoFallbackAfterDrop(t *testing.T) {
	tunables.Update(tunable.Config{IPDelayMs: intPtr(0)})
	c := newCounter(t, func(conn net.Conn) { conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := fetchWebsiteInfo(ctx, c.ln.Addr().String(), "", "", "", nil)
	time.Sleep(150 * time.Millisecond)
	if err == nil {
		t.Fatal("want an error from a dropping server")
	}
	if got := c.n(); got != 1 {
		t.Fatalf("dropping server saw %d page connections, want 1", got)
	}
}

type countingListener struct {
	net.Listener
	onAccept func()
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.onAccept()
	}
	return c, err
}

// /bot tells site owners to write "User-agent: cl0q-node" (or any cl0q bot name) rules — they must apply.
func TestRobotsGroupNamingUsWins(t *testing.T) {
	cases := []struct {
		body    string
		allowed bool
		delay   time.Duration
	}{
		{"User-agent: *\nAllow: /\n\nUser-agent: cl0q-crawler\nDisallow: /\n", false, 0},
		{"User-agent: cl0q-crawler\nCrawl-delay: 10\n\nUser-agent: *\nDisallow: /\n", true, 10 * time.Second},
		{"User-agent: *\nDisallow: /\nCrawl-delay: 3\n", false, 3 * time.Second},
		{"User-agent: Googlebot\nDisallow: /\n", true, 0},
		{"User-agent: cl0q\nUser-agent: bingbot\nDisallow: /\n", false, 0},
		{"User-agent: *\nAllow: /\n\nUser-agent: cl0q-node\nDisallow: /\n", false, 0},
		{"User-agent: *\nDisallow: /\n\nUser-agent: cl0q-node\nCrawl-delay: 5\n", true, 5 * time.Second},
	}
	for i, c := range cases {
		r := parseRobots(c.body)
		if r.allowed != c.allowed || r.delay != c.delay {
			t.Errorf("case %d: got allowed=%v delay=%v, want %v %v", i, r.allowed, r.delay, c.allowed, c.delay)
		}
	}
}

// ---- address guard, redirects, floors, Retry-After and TARGET parsing ----

// A target whose address is not public is never contacted, and the dial guard refuses it even when asked directly.
func TestNonPublicAddressesAreNeverContacted(t *testing.T) {
	c := newCounter(t, func(conn net.Conn) { conn.Close() })
	host := c.ln.Addr().String()
	if r := crawlOne(target{domain: host, ip: "127.0.0.1"}); r.note != "blocked:non-public-address" {
		t.Fatalf("want blocked note, got %+v", r)
	}
	netguard.SetAllowNonPublicForTest(false)
	defer func() { netguard.SetAllowNonPublicForTest(true) }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := fetchWebsiteInfo(ctx, host, "", "", "", nil); !errors.Is(err, netguard.ErrNotPublic) {
		t.Fatalf("want ErrNotPublic, got %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := c.n(); got != 0 {
		t.Fatalf("a non-public server saw %d connections, want 0", got)
	}
}

// redirectSite serves a plain-HTTP site whose handler decides each response; it records request paths and times.
func redirectSite(t *testing.T, h http.HandlerFunc) (host string, paths func() []string, times func() []time.Time) {
	var mu sync.Mutex
	var ps []string
	var ts []time.Time
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ps, ts = append(ps, r.URL.Path), append(ts, time.Now())
		mu.Unlock()
		h(w, r)
	})}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	get := func() ([]string, []time.Time) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), ps...), append([]time.Time(nil), ts...)
	}
	return ln.Addr().String(), func() []string { p, _ := get(); return p }, func() []time.Time { _, x := get(); return x }
}

func fetch(t *testing.T, host string, rules *robotstxt.Rules) *WebsiteInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := fetchWebsiteInfo(ctx, host, "", "", "", rules)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// Same-site redirect hops are followed, paced per site; a hop robots.txt disallows is not; an off-site redirect is
// recorded for discovery instead of followed; chains stop after maxRedirects hops.
func TestRedirectPolicy(t *testing.T) {
	tunables.Update(tunable.Config{IPDelayMs: intPtr(0)})
	tunables.Update(tunable.Config{HostDelayMs: intPtr(150)})
	defer func() { tunables.Update(tunable.Config{HostDelayMs: intPtr(1000)}) }()
	html := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><head><title>ok</title></head><body>hi</body></html>"))
	}

	host, paths, times := redirectSite(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/home", http.StatusFound)
			return
		}
		html(w)
	})
	hostGates = sync.Map{}
	sitePace("127.0.0.1", 0) // crawlOne stamps the site's gate right before the page GET
	if info := fetch(t, host, nil); info.StatusCode != 200 || info.Title != "ok" || info.RedirectCount != 1 {
		t.Fatalf("same-site hop: got status %d title %q redirects %d", info.StatusCode, info.Title, info.RedirectCount)
	}
	if p, ts := paths(), times(); len(p) != 2 || p[1] != "/home" || ts[1].Sub(ts[0]) < 130*time.Millisecond {
		t.Fatalf("hop must be followed and paced ≥ hostDelay: paths %v", p)
	}

	host, paths, _ = redirectSite(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/private/login", http.StatusFound)
	})
	if info := fetch(t, host, robotstxt.Parse("User-agent: *\nDisallow: /private\n")); info.StatusCode != http.StatusFound {
		t.Fatalf("robots-disallowed hop must not be followed, got %d", info.StatusCode)
	}
	if p := paths(); len(p) != 1 {
		t.Fatalf("robots-disallowed hop was requested: %v", p)
	}

	host, paths, _ = redirectSite(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://elsewhere.example/landing", http.StatusMovedPermanently)
	})
	info := fetch(t, host, nil)
	if info.StatusCode != http.StatusMovedPermanently || !info.outDomains["elsewhere.example"] {
		t.Fatalf("off-site redirect must stop and be queued for discovery: status %d, out %v", info.StatusCode, info.outDomains)
	}

	n := 0
	host, paths, _ = redirectSite(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		http.Redirect(w, r, "/hop"+strconv.Itoa(n), http.StatusFound)
	})
	tunables.Update(tunable.Config{HostDelayMs: intPtr(0)})
	if info := fetch(t, host, nil); info.StatusCode != http.StatusFound || info.RedirectCount != maxRedirects {
		t.Fatalf("redirect chain: status %d after %d hops", info.StatusCode, info.RedirectCount)
	}
	if p := paths(); len(p) != maxRedirects {
		t.Fatalf("want %d requests in a redirect chain, got %d", maxRedirects, len(p))
	}
}

// Settings can only make the crawler gentler: zero, negative or oversized values are clamped to the floors.
func TestPolitenessFloors(t *testing.T) {
	en, th, wi, ba, ca := breaker.enabled, breaker.threshold, breaker.window, breaker.base, breaker.cap
	t.Cleanup(func() {
		tunables.Update(tunable.Config{HostDelayMs: intPtr(1000), IPDelayMs: intPtr(1000)})
		breaker.enabled, breaker.threshold, breaker.window, breaker.base, breaker.cap = en, th, wi, ba, ca
	})
	for k, v := range map[string]string{"HOST_DELAY_MS": "0", "IP_DELAY_MS": "-5", "ASN_MAX_PER_BATCH": "500",
		"THROTTLE_RETRIES": "9", "THROTTLE_WAIT_MAX_MS": "600000", "ASN_BREAK_THRESHOLD": "100000",
		"ASN_BREAK_WINDOW_MS": "1", "ASN_BACKOFF_BASE_MS": "1", "ASN_BACKOFF_CAP_MS": "1", "RECRAWL_DAYS": "1",
		"ASN_BACKOFF": "0", "RESPECT_ROBOTS": "0"} {
		t.Setenv(k, v)
	}
	loadPoliteness()
	if tunables.HostDelay() != minHostDelay || tunables.IPDelay() != minIPDelay || perGroupCap != maxPerGroup || throttleRetries != 1 ||
		throttleWaitMax != maxRetryWait || !breaker.enabled || breaker.threshold != breakThreshold ||
		breaker.window != breakWindow || breaker.base != breakBase || breaker.cap != breakCap || recrawlDays() != minRecrawlDays {
		t.Fatalf("floors not enforced: host=%s ip=%s group=%d retries=%d wait=%s breaker=%v/%d/%s/%s/%s recrawl=%d",
			tunables.HostDelay(), tunables.IPDelay(), perGroupCap, throttleRetries, throttleWaitMax, breaker.enabled, breaker.threshold, breaker.window, breaker.base, breaker.cap, recrawlDays())
	}
	t.Setenv("HOST_DELAY_MS", "5000") // gentler than the floor is fine
	t.Setenv("ASN_MAX_PER_BATCH", "3")
	loadPoliteness()
	if tunables.HostDelay() != 5*time.Second || perGroupCap != 3 {
		t.Fatalf("gentler settings must be kept: host=%s group=%d", tunables.HostDelay(), perGroupCap)
	}
}

// A 429/503 is retried once only when its Retry-After fits within throttleWaitMax.
func TestRetryAfterIsRespected(t *testing.T) {
	throttleWaitMax = maxRetryWait
	if d := parseRetryAfter("3600"); d != time.Hour {
		t.Fatalf("Retry-After 3600 parsed as %v", d)
	}
	if parseRetryAfter("") != 0 || parseRetryAfter("soon") != 0 || parseRetryAfter("-5") != 0 {
		t.Fatal("absent or invalid Retry-After must be 0")
	}
	cases := []struct {
		status int
		wait   time.Duration
		budget int
		want   bool
	}{
		{429, 5 * time.Second, 1, true}, {503, 0, 1, true}, {429, time.Hour, 1, false},
		{429, 5 * time.Second, 0, false}, {200, 0, 1, false},
	}
	for i, c := range cases {
		if got := retryThrottled(&WebsiteInfo{StatusCode: c.status, RetryAfter: c.wait}, c.budget); got != c.want {
			t.Errorf("case %d: retry=%v want %v", i, got, c.want)
		}
	}
}

func TestParseTarget(t *testing.T) {
	ok := map[string]string{"example.com": "example.com", "https://www.Example.com/": "example.com",
		"EXAMPLE.com": "example.com", "http://example.com": "example.com", "sub.example.co.uk": "sub.example.co.uk"}
	for in, want := range ok {
		if got, err := parseTarget(in); err != nil || got != want {
			t.Errorf("%q → %q, %v (want %q)", in, got, err, want)
		}
	}
	for _, in := range []string{"example.com/some/path", "https://example.com/a", "example.com:8080", "https://example.com/?q=1",
		"ftp://example.com", "user@example.com", "", "https://"} {
		if got, err := parseTarget(in); err == nil {
			t.Errorf("%q must be rejected, got %q", in, got)
		}
	}
}
