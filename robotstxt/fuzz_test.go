package robotstxt

import (
	"net/url"
	"testing"
)

// FuzzParse throws arbitrary bytes at the parser and asserts the properties
// that must hold for ANY input:
//
//  1. Parse never panics.
//  2. Delay() is always within [0, MaxCrawlDelay] — a hostile robots.txt must
//     not be able to stall a worker.
//  3. Allows()/AllowsPath() are deterministic: parsing the same body twice
//     yields the same verdicts.
//
// Run: go test -fuzz=FuzzParse -fuzztime=60s .
func FuzzParse(f *testing.F) {
	seeds := []string{
		"",
		"User-agent: *\nDisallow: /\n",
		"User-agent: *\nDisallow: /*.php$\nAllow: /public/\nCrawl-delay: 5\n",
		"\ufeffUser-agent: *\nDisallow: /\n",
		"User-agent: cl0q-crawler\nDisallow: /private\nCrawl-delay: 99\n",
		"User-agent: *\nDisallow:\n",
		"# only a comment\nSitemap: https://x.com/sitemap.xml\n",
		"User-agent: *\nCrawl-delay: not-a-number\nDisallow: /\n",
		"User-agent: *\nCrawl-delay: 1e100\nDisallow: /\n",
		"User-agent: *\nCrawl-delay: -5\nDisallow: /\n",
		"User-agent: *\nDisallow: /a*b$c[d(e)f\\g\x00h\n",
		":\n:::\nUser-agent\n",
	}
	urls := []string{
		"https://example.com/",
		"https://example.com/private/page?q=1",
		"https://example.com/a%2Fb",
		"http://[::1]:8080/x",
		"not a url at all",
		"",
	}
	for _, s := range seeds {
		for _, u := range urls {
			f.Add(s, u)
		}
	}
	f.Fuzz(func(t *testing.T, body, rawURL string) {
		r := Parse(body) // must not panic
		if d := r.Delay(); d < 0 || d > MaxCrawlDelay {
			t.Fatalf("Delay() = %v, want within [0, %v] for body %q", d, MaxCrawlDelay, body)
		}
		r2 := Parse(body) // determinism: same body, same verdicts
		if r.Delay() != r2.Delay() {
			t.Fatalf("nondeterministic Delay(): %v vs %v for body %q", r.Delay(), r2.Delay(), body)
		}
		if u, err := url.Parse(rawURL); err == nil && u.Scheme != "" && u.Host != "" {
			if r.Allows(u) != r2.Allows(u) {
				t.Fatalf("nondeterministic Allows(%q) for body %q", rawURL, body)
			}
			return
		}
		// Unparseable URL: still exercise AllowsPath on the raw string.
		if r.AllowsPath(rawURL) != r2.AllowsPath(rawURL) {
			t.Fatalf("nondeterministic AllowsPath(%q) for body %q", rawURL, body)
		}
	})
}
