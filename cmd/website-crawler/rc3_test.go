package main

// Regression tests for the rc3 changes: gate pruning, the circuit breaker, result classification, blacklist lookups,
// word counting, challenge titles, discovery folding and the single robots.txt fetch per host.

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/html"

	"codeberg.org/cl0qsearch/crawler-node/internal/tunable"
)

// Pruning keeps a gate that is still enforcing a gap, and drops (and marks dead) one idle past gateIdle.

func init() {
	if tunables == nil {
		tunables = tunable.New(time.Second, time.Second, 50)
	}
}

func TestPruneGatesNeverShortensAWait(t *testing.T) {
	var gates sync.Map
	throttle(&gates, "k", 200*time.Millisecond)
	pruneGates(&gates, gateIdle)
	start := time.Now()
	throttle(&gates, "k", 200*time.Millisecond)
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("second request after a prune waited only %v; the gap must survive pruning", d)
	}

	v, _ := gates.Load("k")
	g := v.(*hostGate)
	g.mu.Lock()
	g.last = time.Now().Add(-3 * time.Minute)
	g.mu.Unlock()
	pruneGates(&gates, gateIdle)
	if _, ok := gates.Load("k"); ok {
		t.Fatal("a gate idle past gateIdle must be pruned")
	}
	if !g.dead {
		t.Fatal("a pruned gate must be marked dead so a holder looks the key up again")
	}
	throttle(&gates, "k", time.Second) // a worker holding the dead gate would retry; a fresh gate has no wait
	if _, ok := gates.Load("k"); !ok {
		t.Fatal("throttle must create a fresh gate after a prune")
	}
}

func TestBreakerOpensCapsAndRecovers(t *testing.T) {
	b := &asnBreaker{enabled: true, threshold: 2, window: time.Second, base: 40 * time.Millisecond, cap: 80 * time.Millisecond}
	b.fail("g")
	if b.blocked("g") {
		t.Fatal("one failure must not open the breaker")
	}
	b.fail("g")
	if !b.blocked("g") {
		t.Fatal("threshold failures must open the breaker")
	}
	time.Sleep(50 * time.Millisecond)
	if b.blocked("g") {
		t.Fatal("the first cooldown is base")
	}
	for i := 0; i < 6; i++ { // trip repeatedly: the cooldown doubles to the cap and the level stops climbing
		b.fail("g")
		b.fail("g")
		time.Sleep(90 * time.Millisecond)
	}
	v, _ := b.m.Load("g")
	s := v.(*breakState)
	if s.level > 2 {
		t.Fatalf("level kept climbing past the cap (level %d)", s.level)
	}
	b.success("g")
	if s.level != 1 {
		t.Fatalf("a clean result after the cooldown must relax the level, got %d", s.level)
	}
	b.prune(0)
	if _, ok := b.m.Load("g"); ok {
		t.Fatal("an idle, closed group must be pruned")
	}
}

func TestClassify(t *testing.T) {
	blMu.Lock()
	blDomains, blWords = map[string]bool{"bad.example": true}, []string{"casino"}
	blMu.Unlock()
	defer func() { blMu.Lock(); blDomains, blWords = nil, nil; blMu.Unlock() }()

	ok := &WebsiteInfo{StatusCode: 200, Title: "Hello", outDomains: map[string]bool{"other.example": true}}
	cases := []struct {
		r     result
		oc    outcome
		emit  bool
		bad   bool
		ferr  any
		touch bool
	}{
		{result{domain: "a.example", note: "robots_disallow"}, outSkipped, false, false, "robots_disallow", false},
		{result{domain: "a.example", err: errors.New("GET failed: EOF")}, outFailed, false, false, "GET failed: EOF", false},
		{result{domain: "a.example", info: &WebsiteInfo{StatusCode: 304, NotModified: true}}, outUnchanged, false, false, nil, true},
		{result{domain: "a.example", info: &WebsiteInfo{StatusCode: 403, Challenge: "cloudflare"}}, outChallenged, false, false, "challenge:cloudflare", false},
		{result{domain: "a.example", info: ok}, outOK, true, false, nil, false},
		{result{domain: "x.bad.example", info: &WebsiteInfo{StatusCode: 304, NotModified: true}}, outUnchanged, false, true, nil, false},
		{result{domain: "a.example", info: &WebsiteInfo{StatusCode: 200, Title: "best casino"}}, outOK, false, true, nil, false},
	}
	for i, c := range cases {
		out, oc, term := classify(c.r, true)
		if oc != c.oc || out.emit != c.emit || out.bad != c.bad || out.ferr != c.ferr || out.notmod != c.touch {
			t.Errorf("case %d: got oc=%v emit=%v bad=%v ferr=%v notmod=%v", i, oc, out.emit, out.bad, out.ferr, out.notmod)
		}
		if c.bad && term == "" {
			t.Errorf("case %d: a blacklisted row must report what matched", i)
		}
	}
	if out, _, _ := classify(result{domain: "a.example", info: ok}, false); out.emit {
		t.Error("no emit when EMIT_DOMAINS is off")
	}
}

func TestBlDomainHit(t *testing.T) {
	blMu.Lock()
	blDomains = map[string]bool{"bad.example": true}
	blMu.Unlock()
	defer func() { blMu.Lock(); blDomains = nil; blMu.Unlock() }()
	for h, want := range map[string]bool{"bad.example": true, "www.bad.example": true, "a.b.bad.example": true,
		"notbad.example": false, "bad.example.org": false, "example": false} {
		if got := blDomainHit(h); got != want {
			t.Errorf("blDomainHit(%q) = %v, want %v", h, got, want)
		}
	}
}

func TestCountWordsMatchesFields(t *testing.T) {
	for _, s := range []string{"", "   ", "one", " two  words ", "tab\tand\nnewline", "unicode nbsp em space", "日本語 テキスト"} {
		if got, want := countWords(s), len(strings.Fields(s)); got != want {
			t.Errorf("countWords(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestChallengeTitles(t *testing.T) {
	for title, want := range map[string]string{
		"Just a moment...":                       "cloudflare",
		"Just a moment…":                         "cloudflare",
		"Just a moment of calm: a photo essay":   "",
		"Verifying you are human. This may take": "challenge",
		"Checking your browser before accessing": "cloudflare",
		"One moment, please...":                  "challenge",
		"Our guide: just a moment":               "",
	} {
		if got := detectChallenge(title, nil); got != want {
			t.Errorf("detectChallenge(%q) = %q, want %q", title, got, want)
		}
	}
}

// Discovery folds subdomains to their registrable site — including under second-level registries rc2 got wrong —
// and never emits IPs or bare public suffixes.
func TestDiscoveryFolding(t *testing.T) {
	crawlSubdomains.Store(false)
	doc, err := html.Parse(strings.NewReader(`<a href="https://news.nation.co.ke/x">a</a><a href="http://co.ke/">b</a>
		<a href="http://192.0.2.9/">c</a><a href="https://foo.blogspot.com/">d</a><a href="https://www.punchng.com.ng/">e</a>
		<a href="mailto:x@y.example">f</a><a href="/local">g</a>`))
	if err != nil {
		t.Fatal(err)
	}
	info := &WebsiteInfo{}
	parseHTMLInfo(doc, info, "self.example")
	want := map[string]bool{"nation.co.ke": true, "blogspot.com": true, "punchng.com.ng": true}
	if len(info.outDomains) != len(want) {
		t.Fatalf("outDomains = %v, want %v", info.outDomains, want)
	}
	for d := range want {
		if !info.outDomains[d] {
			t.Errorf("missing %s in %v", d, info.outDomains)
		}
	}
}

// Concurrent callers for one host share a single robots.txt request.
func TestRobotsForSingleFetch(t *testing.T) {
	tunables.Update(tunable.Config{IPDelayMs: intPtr(0)})
	var hits atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(100 * time.Millisecond)
		w.Write([]byte("User-agent: *\nDisallow: /private\n"))
	})}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	robotsCache = sync.Map{}
	host := ln.Addr().String()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); robotsFor(host, "") }()
	}
	wg.Wait()
	// https to a plain-http server fails as "not TLS" without reaching the handler; only the http:// GET counts.
	if n := hits.Load(); n != 1 {
		t.Fatalf("robots.txt requested %d times, want 1", n)
	}
}
