package robotstxt

import (
	"strings"
	"testing"
)

// TestMatcherEquivalence is the D3 acceptance gate: when the custom wildcard
// matcher replaces the regex engine, it must agree with the current
// regex-based implementation on every input.
//
// This test is written NOW (before the matcher exists) so the contract is
// fixed before implementation begins. It currently validates the regex
// implementation against itself as a sanity check; once the custom matcher
// lands, it will compare custom-vs-regex.
//
// Equivalence corpus: wildcards in every position, trailing $ anchors,
// empty patterns, literal regex metacharacters that must NOT be special,
// percent-encoded paths, and query strings.
var matcherEquivalenceCases = []struct {
	pattern string
	paths   []string
}{
	{"/admin/", []string{"/admin/", "/admin/page", "/administrator", "/"}},
	{"/*.php$", []string{"/index.php", "/a/b.php", "/index.php/", "/index.phpx"}},
	{"/a*b", []string{"/ab", "/axxxb", "/a/b", "/b", "/a"}},
	{"/*/private/*", []string{"/x/private/y", "/private/y", "/x/private/", "/"}},
	{"/search?q=*", []string{"/search?q=hello", "/search", "/search?q=", "/search?q=a/b"}},
	{"/exact$", []string{"/exact", "/exact/", "/exactx"}},
	{"/a.c", []string{"/a.c", "/abc", "/axc"}},          // . is literal, not regex-any
	{"/a+b", []string{"/a+b", "/aab", "/ab"}},           // + is literal
	{"/a(b)", []string{"/a(b)", "/ab"}},                 // parens are literal
	{"/café", []string{"/caf%C3%A9", "/café", "/cafe"}}, // unicode percent-encoding
	{"/*", []string{"/", "/anything/at/all"}},
	{"$", []string{"/", "/x"}}, // trailing-$ with empty prefix anchors end... of "^" = start only
}

// TestMatcherEquivalence runs the corpus against BOTH the new custom matcher
// and the legacy regex fixture, requiring byte-for-byte identical verdicts.
// This is Claude's D3 condition 3: the new matcher must preserve semantics exactly.
func TestMatcherEquivalence(t *testing.T) {
	for _, tc := range matcherEquivalenceCases {
		r := Parse("User-agent: *\nDisallow: " + tc.pattern + "\n")
		// Extract the compiled wildcard from the parsed rule.
		if len(r.rules) != 1 {
			t.Fatalf("pattern %q: expected 1 rule, got %d", tc.pattern, len(r.rules))
		}
		w := r.rules[0].pat
		for _, p := range tc.paths {
			gotNew := w.match(p)
			gotLegacy := legacyMatch(tc.pattern, p)
			if gotNew != gotLegacy {
				t.Errorf("pattern %q path %q: new=%v legacy=%v — DIVERGENCE", tc.pattern, p, gotNew, gotLegacy)
			}
			// Also verify AllowsPath agrees (integration check).
			gotAllows := r.AllowsPath(p)
			// AllowsPath returns !disallowed; pattern is a Disallow rule.
			if gotAllows == gotNew {
				t.Errorf("pattern %q path %q: AllowsPath=%v but match=%v — INCONSISTENT", tc.pattern, p, gotAllows, gotNew)
			}
		}
	}
}

// FuzzMatcherEquivalence throws random patterns and paths at BOTH the new
// custom matcher and the legacy regex fixture, requiring identical verdicts.
// This is Claude's D3 condition 3 over a large random corpus.
//
// Run: go test -run=FuzzMatcherEquivalence -fuzz=FuzzMatcherEquivalence -fuzztime=60s .
func FuzzMatcherEquivalence(f *testing.F) {
	seeds := []string{
		"User-agent: *\nDisallow: /*.php$\n",
		"User-agent: *\nDisallow: /a*b$c\n",
		"User-agent: *\nAllow: /public/*\nDisallow: /*\n",
		"User-agent: *\nDisallow: /x?y=z*\n",
	}
	paths := []string{"/", "/a", "/a/b/c", "/x.php", "/axxxb", "/public/f"}
	for _, s := range seeds {
		for _, p := range paths {
			f.Add(s, p)
		}
	}
	f.Fuzz(func(t *testing.T, body, path string) {
		// Restrict to printable ASCII to keep patterns meaningful.
		clean := func(s string) string {
			var b strings.Builder
			for _, r := range s {
				if r >= 32 && r < 127 {
					b.WriteRune(r)
				}
			}
			return b.String()
		}
		body, path = clean(body), clean(path)
		r := Parse(body) // must not panic
		_ = r.AllowsPath(path)

		// Differential: for each Allow/Disallow line, the new wildcard matcher
		// must agree with the legacy regex fixture.
		for _, line := range strings.Split(body, "\n") {
			if i := strings.IndexByte(line, '#'); i >= 0 {
				line = line[:i]
			}
			line = strings.TrimSpace(line)
			idx := strings.IndexByte(line, ':')
			if idx < 0 {
				continue
			}
			field := strings.ToLower(strings.TrimSpace(line[:idx]))
			if field != "allow" && field != "disallow" {
				continue
			}
			pat := strings.TrimSpace(line[idx+1:])
			if pat == "" {
				continue
			}
			w, ok := compileWildcard(escapePattern(pat))
			if !ok {
				continue // skipped by cap — legacy would also be skipped
			}
			if got, want := w.match(path), legacyMatch(pat, path); got != want {
				t.Fatalf("DIVERGENCE: pattern %q path %q: new=%v legacy=%v", pat, path, got, want)
			}
		}
	})
}
