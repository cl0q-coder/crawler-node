package main

// Politeness tests: robots.txt path rules and group precedence, Crawl-delay cap, URLs never fetched.
//   go test ./cmd/domain-crawler

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"cl0q/botid"
	"cl0q/crawlutil"
	"cl0q/netguard"
)

// The fake servers below listen on 127.0.0.1, which netguard refuses; the guard is switched off inside this test
// binary only. main() normally sets the User-Agent.
func init() {
	netguard.SetAllowNonPublicForTest(true)
	userAgent, _ = botid.UserAgent("test-node")
}

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }

func TestRobotsPathRules(t *testing.T) {
	r := parseRobots(`
User-agent: Googlebot
Disallow: /

User-agent: *
Disallow: /private/
Disallow: /*.php$
Allow: /private/public-page
Disallow: /search
Crawl-delay: 5
`)
	cases := map[string]bool{
		"https://x.com/":                     true,
		"https://x.com/about":                true,
		"https://x.com/private/":             false,
		"https://x.com/private/secret":       false,
		"https://x.com/private/public-page":  true, // longer Allow beats shorter Disallow
		"https://x.com/index.php":            false,
		"https://x.com/index.php5":           true, // $ anchors the end
		"https://x.com/search":               false,
		"https://x.com/searching-for-things": false, // prefix match, like every major crawler
	}
	for u, want := range cases {
		if got := r.allows(mustURL(u)); got != want {
			t.Errorf("%s: allows=%v want %v", u, got, want)
		}
	}
	if r.Delay() != 5*time.Second {
		t.Errorf("crawl-delay %v, want 5s", r.Delay())
	}
}

func TestRobotsOurGroupWins(t *testing.T) {
	r := parseRobots("User-agent: *\nAllow: /\n\nUser-agent: cl0q-crawler\nDisallow: /\n")
	if r.allows(mustURL("https://x.com/page")) {
		t.Error("a group naming cl0q-crawler must apply to nodes too")
	}
	r = parseRobots("User-agent: *\nAllow: /\n\nUser-agent: cl0q-node\nDisallow: /private\n")
	if r.allows(mustURL("https://x.com/private/a")) || !r.allows(mustURL("https://x.com/public")) {
		t.Error("a group naming cl0q-node must apply")
	}
	r = parseRobots("User-agent: cl0q-discovery\nUser-agent: otherbot\nDisallow: /blog\nCrawl-delay: 99\n")
	if r.allows(mustURL("https://x.com/blog/post")) || !r.allows(mustURL("https://x.com/")) {
		t.Error("multi-agent group naming us must apply")
	}
	if r.Delay() != 30*time.Second {
		t.Errorf("crawl-delay must cap at 30s, got %v", r.Delay())
	}
	if !parseRobots("User-agent: *\nDisallow:\n").allows(mustURL("https://x.com/anything")) {
		t.Error("empty Disallow allows everything")
	}
	if parseRobots("User-agent: *\nDisallow: /\nAllow: /$\n").allows(mustURL("https://x.com/deep")) {
		t.Error("blanket disallow with only the homepage allowed must block deep pages")
	}
}

func TestSkipURL(t *testing.T) {
	skip := []string{"https://x.com/wp-login.php", "https://x.com/wp-admin/", "https://x.com/account/settings",
		"https://x.com/cart", "https://x.com/search?q=a", "https://x.com/page?id=2", "https://x.com/file.pdf",
		"https://x.com/img/logo.PNG", "https://x.com/Login", "https://x.com/user/login", "https://x.com/cgi-bin/x"}
	keep := []string{"https://x.com/", "https://x.com/blog/accountability-in-tech", "https://x.com/about/",
		"https://x.com/admins-are-people", "https://x.com/research", "https://x.com/news/2026/10/post.html"}
	for _, s := range skip {
		if !skipURL(mustURL(s)) {
			t.Errorf("should skip %s", s)
		}
	}
	for _, s := range keep {
		if skipURL(mustURL(s)) {
			t.Errorf("should keep %s", s)
		}
	}
}

// A blocked or missing verdict must not panic when its Crawl-delay or rules are read.
func TestNilVerdicts(t *testing.T) {
	var r *robotRules
	if !r.allows(mustURL("https://x.com/")) {
		t.Error("nil verdict allows everything")
	}
	b := &robotRules{blocked: true}
	if b.Delay() != 0 || !b.allows(mustURL("https://x.com/")) {
		t.Error("blocked verdict has no rules of its own")
	}
	if crawlutil.HTTPSUnavailable(nil) {
		t.Error("nil error is not 'https unavailable'")
	}
}

// fakeSite counts requests (path + arrival time) on a plain-HTTP server; handler decides the response.
func fakeSite(t *testing.T, h http.HandlerFunc) (host string, got func() ([]string, []time.Time)) {
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
	return ln.Addr().String(), func() ([]string, []time.Time) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), ps...), append([]time.Time(nil), ts...)
	}
}

// The robots.txt http fallback is paced: https fails (plain-HTTP server), then http comes at least gap later.
func TestRobotsFallbackIsPaced(t *testing.T) {
	var conns []time.Time
	var mu sync.Mutex
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("User-agent: *\nDisallow: /private\n"))
	}), ConnState: func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			mu.Lock()
			conns = append(conns, time.Now())
			mu.Unlock()
		}
	}}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	r := fetchRobotsRules(ln.Addr().String(), 200*time.Millisecond)
	if r.blocked || r.allows(mustURL("https://x.com/private/a")) {
		t.Fatalf("want parsed rules, got blocked=%v", r.blocked)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(conns) != 2 || conns[1].Sub(conns[0]) < 180*time.Millisecond {
		t.Fatalf("want 2 connections ≥200ms apart (https then http), got %d", len(conns))
	}
}

// Same-host redirects are followed after the gap; a redirect to a skipped URL, a robots-disallowed path or another
// host is not followed but handed back as a link for the BFS.
func TestPageRedirects(t *testing.T) {
	host, got := fakeSite(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			http.Redirect(w, r, "/home", http.StatusFound)
		case "/home":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<a href="https://example.org/">x</a>`))
		case "/to-login":
			http.Redirect(w, r, "/login", http.StatusFound)
		case "/to-private":
			http.Redirect(w, r, "/private/x", http.StatusFound)
		case "/offsite":
			http.Redirect(w, r, "https://other.example/landing", http.StatusMovedPermanently)
		}
	})
	rules := parseRobots("User-agent: *\nDisallow: /private\n")
	gap := 150 * time.Millisecond
	if links := safeCrawl("http://"+host+"/", rules, gap); len(links) != 1 || links[0] != "https://example.org/" {
		t.Fatalf("same-host hop should be followed to /home, got links %v", links)
	}
	ps, ts := got()
	if len(ps) != 2 || ps[1] != "/home" || ts[1].Sub(ts[0]) < 130*time.Millisecond {
		t.Fatalf("hop must be paced ≥ gap: %v", ps)
	}
	for path, want := range map[string]string{"/to-login": "/login", "/to-private": "/private/x", "/offsite": "https://other.example/landing"} {
		links := safeCrawl("http://"+host+path, rules, 0)
		if len(links) != 1 || (links[0] != want && !strings.HasSuffix(links[0], want)) {
			t.Errorf("%s: want the redirect target handed back (%s), got %v", path, want, links)
		}
	}
	ps, _ = got()
	for _, p := range ps {
		if p == "/login" || p == "/private/x" {
			t.Errorf("%s must never be requested", p)
		}
	}
}

func TestIPLiteralLinksAndCase(t *testing.T) {
	if d, _ := resolveLink("http://169.254.169.254/latest/", "https://a.example/"); d != "" {
		t.Error("IP-literal links are not sites")
	}
	if d, full := resolveLink("https://EXAMPLE.com/Path", "https://a.example/"); d != "example.com" || full != "https://example.com/Path" {
		t.Errorf("host must be lower-cased (path kept): %q %q", d, full)
	}
}

// Robots.txt redirects stay on the registrable site, which rc2 got wrong under second-level registries.
func TestSameSite(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"example.com", "www.example.com", true}, {"a.example.com", "b.example.com", true},
		{"a.co.ke", "b.co.ke", false}, {"news.nation.co.ke", "nation.co.ke", true},
		{"192.0.2.1", "192.0.2.1", true}, {"192.0.2.1", "192.0.2.2", false}, {"example.com", "example.org", false},
	} {
		if got := sameSite(c.a, c.b); got != c.want {
			t.Errorf("sameSite(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// A non-HTML page is read to the end (up to 1 MiB), so the next page of the same site reuses the connection.
func TestNonHTMLBodyIsDrained(t *testing.T) {
	var mu sync.Mutex
	conns := 0
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write(make([]byte, 64<<10))
	}), ConnState: func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			mu.Lock()
			conns++
			mu.Unlock()
		}
	}}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	host := ln.Addr().String()
	safeCrawl("http://"+host+"/a", nil, 0)
	safeCrawl("http://"+host+"/b", nil, 0)
	mu.Lock()
	defer mu.Unlock()
	if conns != 1 {
		t.Fatalf("two pages opened %d connections; the first body must be drained so the connection is reused", conns)
	}
}
