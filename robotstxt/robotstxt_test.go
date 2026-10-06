package robotstxt

import (
	"net/url"
	"testing"
	"time"
)

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }

func TestPathRules(t *testing.T) {
	r := Parse(`
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
		"https://x.com/a?q=1":                true,
	}
	for u, want := range cases {
		if got := r.Allows(mustURL(u)); got != want {
			t.Errorf("%s: Allows=%v want %v", u, got, want)
		}
	}
	if r.Delay() != 5*time.Second {
		t.Errorf("crawl-delay %v, want 5s", r.Delay())
	}
}

// Root verdict + delay for the cases cl0q.com/bot describes: a group naming any cl0q bot beats "*".
func TestGroupPrecedence(t *testing.T) {
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
		{"User-agent: *\nDisallow: /\n\nUser-agent: cl0q-discovery\n", true, 0}, // an empty group naming us still wins
		{"User-agent: *\nDisallow: /*\n", false, 0},
		{"User-agent: *\nDisallow:\n", true, 0},
		{"User-agent: *\nDisallow: /\nAllow: /$\n", true, 0},
		{"", true, 0},
	}
	for i, c := range cases {
		r := Parse(c.body)
		if got := r.AllowsPath("/"); got != c.allowed || r.Delay() != c.delay {
			t.Errorf("case %d: got allowed=%v delay=%v, want %v %v", i, got, r.Delay(), c.allowed, c.delay)
		}
	}
	if Parse("User-agent: *\nDisallow: /\nAllow: /$\n").Allows(mustURL("https://x.com/deep")) {
		t.Error("blanket disallow with only the homepage allowed must block deep pages")
	}
}

func TestDelayCapAndMerge(t *testing.T) {
	r := Parse("User-agent: cl0q-discovery\nUser-agent: otherbot\nDisallow: /blog\nCrawl-delay: 99\n")
	if r.Allows(mustURL("https://x.com/blog/post")) || !r.Allows(mustURL("https://x.com/")) {
		t.Error("multi-agent group naming us must apply")
	}
	if r.Delay() != MaxCrawlDelay {
		t.Errorf("crawl-delay must cap at %v, got %v", MaxCrawlDelay, r.Delay())
	}
	var nilRules *Rules
	if !nilRules.Allows(mustURL("https://x.com/")) || nilRules.Delay() != 0 {
		t.Error("nil rules allow everything")
	}
}

// Lines that belong to no group (Sitemap:, Host:, unknown fields) must not split consecutive User-agent lines.
func TestNonGroupLinesDontSplitGroups(t *testing.T) {
	r := Parse("User-agent: cl0q-crawler\nSitemap: https://x.com/sitemap.xml\nUser-agent: otherbot\nDisallow: /private\n")
	if r.Allows(mustURL("https://x.com/private/a")) {
		t.Error("the Disallow after a Sitemap: line still belongs to the group naming cl0q-crawler")
	}
	r = Parse("User-agent: *\nHost: x.com\nDisallow: /tmp\n")
	if r.Allows(mustURL("https://x.com/tmp/1")) {
		t.Error("a Host: line inside a group must not end it")
	}
}

// A UTF-8 BOM at the start of the file must not break parsing of the first
// group (RFC 9309 §2.1): without stripping, the first field parses as
// "\ufeffuser-agent" and a "Disallow: /" file wrongly allows everything.
func TestParseStripsBOM(t *testing.T) {
	r := Parse("\ufeffUser-agent: *\nDisallow: /\n")
	if r.Allows(mustURL("https://x.com/anything")) {
		t.Error("BOM-prefixed robots.txt with Disallow: / must not allow paths")
	}
	// A file without a BOM behaves identically.
	r2 := Parse("User-agent: *\nDisallow: /\n")
	if r2.Allows(mustURL("https://x.com/anything")) {
		t.Error("plain robots.txt with Disallow: / must not allow paths")
	}
}

// Non-ASCII path prefixes must match the percent-encoded path the way the
// regex path does (bug: plainPrefix returned the raw pattern, so "/café"
// never matched "/caf%C3%A9").
func TestNonASCIIPrefix(t *testing.T) {
	r := Parse(`
User-agent: *
Disallow: /café
Disallow: /naïve/
Allow: /café/public
`)
	cases := map[string]bool{
		"https://x.com/café":             false, // raw non-ASCII URL
		"https://x.com/caf%C3%A9":        false, // pre-encoded URL
		"https://x.com/caf%c3%a9":        false, // lowercase hex -> normalized
		"https://x.com/café/public":      true,  // longer Allow wins
		"https://x.com/café/public/page": true,
		"https://x.com/naïve/":           false,
		"https://x.com/naïve/deep":       false,
		"https://x.com/cafe":             true, // no accent, no match
		"https://x.com/other":            true,
	}
	for u, want := range cases {
		if got := r.Allows(mustURL(u)); got != want {
			t.Errorf("%s: Allows=%v want %v", u, got, want)
		}
	}
}
