package crawlutil

import (
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
)

func TestRegistrable(t *testing.T) {
	cases := map[string]string{
		"example.com":          "example.com",
		"www.Example.COM.":     "example.com",
		"blog.acme.com":        "acme.com",
		"a.b.acme.com":         "acme.com",
		"a.bbc.co.uk":          "bbc.co.uk",
		"bbc.co.uk":            "bbc.co.uk",
		"nation.co.ke":         "nation.co.ke", // rc2 folded these to "co.ke"
		"news.nation.co.ke":    "nation.co.ke",
		"punchng.com.ng":       "punchng.com.ng",
		"x.y.co.th":            "y.co.th",
		"foo.blogspot.com":     "blogspot.com", // private suffix: keep folding platform subdomains
		"user.github.io":       "github.io",
		"co.ke":                "", // a public suffix is not a site
		"com":                  "",
		"localhost":            "",
		"192.0.2.7":            "",
		"":                     "",
		"cl0q-node-qa.example": "cl0q-node-qa.example", // unlisted TLD: last label is the suffix
	}
	for in, want := range cases {
		if got := Registrable(in); got != want {
			t.Errorf("Registrable(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsSubdomainAndSuffix(t *testing.T) {
	for h, want := range map[string]bool{"example.com": false, "www.example.com": false, "blog.example.com": true,
		"nation.co.ke": false, "news.nation.co.ke": true, "foo.blogspot.com": true, "co.ke": false} {
		if got := IsSubdomain(h); got != want {
			t.Errorf("IsSubdomain(%q) = %v, want %v", h, got, want)
		}
	}
	for h, want := range map[string]bool{"co.ke": true, "com.ng": true, "com": true, "nation.co.ke": false,
		"blogspot.com": false, "": false} {
		if got := IsPublicSuffix(h); got != want {
			t.Errorf("IsPublicSuffix(%q) = %v, want %v", h, got, want)
		}
	}
}

func TestHTTPSUnavailable(t *testing.T) {
	if HTTPSUnavailable(nil) {
		t.Error("nil is not 'https unavailable'")
	}
	for _, s := range []string{"dial tcp 192.0.2.1:443: connect: connection refused", "x509: certificate signed by unknown authority",
		"http: server gave HTTP response to HTTPS client"} {
		if !HTTPSUnavailable(errors.New(s)) {
			t.Errorf("want fallback for %q", s)
		}
	}
	for _, s := range []string{"EOF", "read: connection reset by peer", "context deadline exceeded"} {
		if HTTPSUnavailable(errors.New(s)) {
			t.Errorf("want NO fallback for %q", s)
		}
	}
}

// C3: property-style tests for the public-suffix logic. These assert structural
// invariants that must hold for EVERY host, not just the table cases above:
// idempotence, suffix-anchoring, and "never returns a public suffix itself".
// Deterministic: the adversarial corpus is fixed, no randomness, no network.
func TestRegistrableProperties(t *testing.T) {
	// Adversarial corpus: deep nesting, mixed case, trailing dots, www-stripping,
	// private suffixes, multi-label public suffixes, IDN-ish and numeric labels.
	hosts := []string{
		"a.b.c.d.e.example.com", "WWW.a.B.Cc.co.UK.", "www.www.example.com",
		"foo.bar.blogspot.com", "a.b.user.github.io", "deep.nest.news.bbc.co.uk",
		"xn--nxasmq6b.example", "123.456.example.com", "a-.example.com",
		"example-.com", "sub_domain.example.com", "a.b.c.d.e.f.g.h.i.j.k.l.m.n.o.p.example.org",
		"co.ke", "com", "a.co.ke", "b.a.co.ke", "tokyo.jp", "x.y.tokyo.jp",
		"com.ng", "a.com.ng", "s3.amazonaws.com", "bucket.s3.amazonaws.com",
		"appspot.com", "x.appspot.com", "192.0.2.1", "::1", "localhost", "",
		"a", ".", "..", "a..com", ".example.com", "example.com.",
	}
	for _, h := range hosts {
		r := Registrable(h)
		if r == "" {
			continue
		}
		// 1. Idempotence: the registrable site of a registrable site is itself.
		if r2 := Registrable(r); r2 != r {
			t.Errorf("Registrable(%q) = %q, but Registrable(%q) = %q (not idempotent)", h, r, r, r2)
		}
		// 2. Suffix anchoring: r is h itself or h ends with "."+r.
		nh := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
		nh = strings.TrimPrefix(nh, "www.")
		if nh != r && !strings.HasSuffix(nh, "."+r) {
			t.Errorf("Registrable(%q) = %q, which is not a suffix anchor of %q", h, r, nh)
		}
		// 3. Never a public suffix itself: r always has one more label than its ICANN suffix.
		if IsPublicSuffix(r) {
			t.Errorf("Registrable(%q) = %q, which is itself a public suffix", h, r)
		}
		// 4. Not an IP and has at least two labels.
		if net.ParseIP(r) != nil || !strings.Contains(r, ".") {
			t.Errorf("Registrable(%q) = %q, not a plausible site", h, r)
		}
	}
}

// Generated sweep: every 1-3 label combination over a small alphabet must satisfy
// the same invariants (catches normalization edge cases the fixed corpus misses).
func TestRegistrableGenerated(t *testing.T) {
	labels := []string{"a", "bb", "0", "x-y", "com", "co", "ke", "blogspot"}
	var hosts []string
	for _, l1 := range labels {
		hosts = append(hosts, l1)
		for _, l2 := range labels {
			hosts = append(hosts, l1+"."+l2)
			for _, l3 := range labels {
				hosts = append(hosts, l1+"."+l2+"."+l3)
			}
		}
	}
	for _, h := range hosts {
		r := Registrable(h)
		if r == "" {
			continue
		}
		if r2 := Registrable(r); r2 != r {
			t.Errorf("not idempotent: Registrable(%q) = %q, Registrable(%q) = %q", h, r, r, r2)
		}
		if IsPublicSuffix(r) {
			t.Errorf("public suffix returned: Registrable(%q) = %q", h, r)
		}
	}
}

// Normalization is total: case, whitespace, trailing dots and www-stripping never
// change the answer.
func TestRegistrableNormalization(t *testing.T) {
	base := Registrable("blog.example.com")
	for _, v := range []string{"BLOG.EXAMPLE.COM", " blog.example.com ", "blog.example.com.",
		"www.blog.example.com", "WWW.Blog.Example.COM."} {
		if got := Registrable(v); got != base {
			t.Errorf("Registrable(%q) = %q, want %q", v, got, base)
		}
	}
}

// C7: benchmark the hot path (called per discovered host during crawls).
func BenchmarkRegistrable(b *testing.B) {
	hosts := []string{"blog.acme.com", "a.b.c.d.example.org", "news.nation.co.ke",
		"foo.blogspot.com", "deep.nest.news.bbc.co.uk", "example.com"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Registrable(hosts[i%len(hosts)])
	}
}

func BenchmarkIsSubdomain(b *testing.B) {
	hosts := []string{"blog.acme.com", "acme.com", "news.nation.co.ke", "nation.co.ke"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		IsSubdomain(hosts[i%len(hosts)])
	}
}

// C8: race stress — the suffix helpers are called from every crawl worker.
// Run with: go test -race ./crawlutil/
func TestRegistrableConcurrent(t *testing.T) {
	hosts := []string{"blog.acme.com", "a.b.c.d.example.org", "news.nation.co.ke",
		"foo.blogspot.com", "co.ke", "192.0.2.7", "WWW.Example.COM."}
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				h := hosts[(w+i)%len(hosts)]
				_ = Registrable(h)
				_ = IsSubdomain(h)
				_ = IsPublicSuffix(h)
			}
		}(w)
	}
	wg.Wait()
}
