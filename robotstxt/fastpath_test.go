package robotstxt

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// probePaths exercises plain prefixes, wildcards, $-anchors, query strings
// and deep paths across the fixture corpus.
var probePaths = []string{
	"/", "/about", "/contact", "/sitemap.xml",
	"/search", "/search?q=hello", "/searching",
	"/wp-admin/", "/wp-admin/admin-ajax.php",
	"/private/", "/private/secret",
	"/user/john", "/user/john/followers",
	"/cart/", "/checkout/payment",
	"/index.php", "/index.php5",
	"/page?utm_source=x", "/download/file.pdf",
	"/api/public/v1", "/api/internal/debug",
	"/blog/", "/blog/drafts/post1",
	"/tmp/x", "/feed", "/login?next=/",
	"/a/b/c/d/e",
}

// loadCorpus parses every fixture in testdata/robots.
func loadCorpus(t *testing.T) map[string]*Rules {
	t.Helper()
	files, err := filepath.Glob("testdata/robots/*.txt")
	if err != nil || len(files) == 0 {
		t.Fatal("testdata/robots corpus missing")
	}
	sort.Strings(files)
	out := make(map[string]*Rules, len(files))
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		out[filepath.Base(f)] = Parse(string(body))
	}
	return out
}

// forceWildcard returns a copy of r with every rule on the wildcard-matcher
// path, so the fast path can be benchmarked and differential-tested against
// the matcher over identical rule sets.
func forceWildcard(r *Rules) *Rules {
	out := &Rules{delay: r.delay, rules: make([]rule, len(r.rules))}
	for i, rl := range r.rules {
		if rl.prefix != "" {
			// Prefixes come from patterns with no '*' and no trailing '$'
			// (already escaped), so this always yields a single unanchored
			// segment — the same wildcard Parse would have built.
			rl.pat, _ = compileWildcard(rl.prefix)
			rl.prefix = ""
		}
		out.rules[i] = rl
	}
	return out
}

// TestFastPathMatchesWildcard requires the prefix fast path to return the
// same verdict as the wildcard matcher for every fixture and probe path.
func TestFastPathMatchesWildcard(t *testing.T) {
	for name, r := range loadCorpus(t) {
		wc := forceWildcard(r)
		for _, p := range probePaths {
			if got, want := r.AllowsPath(p), wc.AllowsPath(p); got != want {
				t.Errorf("%s %q: fast=%v wildcard=%v", name, p, got, want)
			}
		}
	}
}

var benchSinkAllow bool

// BenchmarkAllows compares the prefix fast path against the wildcard matcher
// over the full fixture corpus (50 files). Every iteration checks every probe
// path against every fixture's rules, so the per-iteration cost is dominated
// by rule matching, not setup.
func BenchmarkAllows(b *testing.B) {
	files, err := filepath.Glob("testdata/robots/*.txt")
	if err != nil || len(files) == 0 {
		b.Fatal("testdata/robots corpus missing")
	}
	sort.Strings(files)
	type pair struct{ fast, wildcard *Rules }
	var corpus []pair
	totalRules := 0
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			b.Fatalf("read %s: %v", f, err)
		}
		r := Parse(string(body))
		totalRules += len(r.rules)
		corpus = append(corpus, pair{r, forceWildcard(r)})
	}
	b.Logf("%d fixtures, %d rules, %d probe paths", len(files), totalRules, len(probePaths))
	b.Run("FastPath", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for _, c := range corpus {
				for _, p := range probePaths {
					benchSinkAllow = c.fast.AllowsPath(p)
				}
			}
		}
	})
	b.Run("WildcardPath", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for _, c := range corpus {
				for _, p := range probePaths {
					benchSinkAllow = c.wildcard.AllowsPath(p)
				}
			}
		}
	})
}
