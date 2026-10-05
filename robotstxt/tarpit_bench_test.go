package robotstxt

import (
	"fmt"
	"strings"
	"testing"
)

// tarpitBody generates a hostile robots.txt: nRules rules, each with
// nWildcards wildcards. This is the D3 (P1 #1) attack shape: pattern()
// compiles nRules regexes at parse time, and AllowsPath runs every regex
// on every URL check.
func tarpitBody(nRules, nWildcards int) string {
	var b strings.Builder
	b.WriteString("User-agent: *\n")
	for i := 0; i < nRules; i++ {
		p := fmt.Sprintf("/section%d/", i)
		for w := 0; w < nWildcards; w++ {
			p += fmt.Sprintf("seg%d*", w)
		}
		if i%2 == 0 {
			p += "$"
		}
		field := "Disallow"
		if i%3 == 0 {
			field = "Allow"
		}
		fmt.Fprintf(&b, "%s: %s\n", field, p)
	}
	return b.String()
}

// probeURLs are representative crawler URLs to test against the tarpit.
var probeURLs = []string{
	"/",
	"/section42/seg0abc/seg1def/page.html",
	"/section499/seg0x/seg1y/seg2z/seg3w/deep/path?q=1",
	"/unmatched/path/that/hits/nothing",
	"/section0/",
}

// BenchmarkTarpitParse measures how long Parse takes on a hostile robots.txt.
// This is paid once per robots.txt fetch.
func BenchmarkTarpitParse(b *testing.B) {
	for _, tc := range []struct{ rules, wildcards int }{
		{100, 5},
		{500, 10},
		{1000, 10},
	} {
		body := tarpitBody(tc.rules, tc.wildcards)
		b.Run(fmt.Sprintf("rules=%d_wildcards=%d", tc.rules, tc.wildcards), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = Parse(body)
			}
		})
	}
}

// BenchmarkTarpitAllowsPath measures the per-URL cost of AllowsPath against
// a hostile rule set. This is paid for EVERY URL the crawler considers —
// the actual tarpit.
func BenchmarkTarpitAllowsPath(b *testing.B) {
	for _, tc := range []struct{ rules, wildcards int }{
		{100, 5},
		{500, 10},
		{1000, 10},
	} {
		r := Parse(tarpitBody(tc.rules, tc.wildcards))
		b.Run(fmt.Sprintf("rules=%d_wildcards=%d", tc.rules, tc.wildcards), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = r.AllowsPath(probeURLs[i%len(probeURLs)])
			}
		})
	}
}

// BenchmarkRealisticAllowsPath is the control: a normal-sized robots.txt
// (20 rules, few wildcards) so we can compare tarpit vs typical cost.
func BenchmarkRealisticAllowsPath(b *testing.B) {
	body := `User-agent: *
Disallow: /admin/
Disallow: /private/
Disallow: /*.php$
Disallow: /tmp/*
Allow: /public/
Disallow: /search?q=*
Crawl-delay: 1
`
	r := Parse(body)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.AllowsPath(probeURLs[i%len(probeURLs)])
	}
}
