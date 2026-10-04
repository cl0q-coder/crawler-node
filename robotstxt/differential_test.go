package robotstxt

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"

	gro "github.com/jimsmart/grobotstxt"
)

// TestDifferentialVsGoogle compares our verdicts against Google's reference
// parser (github.com/jimsmart/grobotstxt, a Go port of google/robotstxt) over
// the corpus in testdata/. Every body uses only "User-agent: *" groups so both
// parsers select the same group; group precedence for named cl0q bots is
// cl0q-specific and covered by TestGroupPrecedence instead.
//
// Intentional divergences (documented, not failures):
//   - None currently. Two divergences found during development were real bugs
//     and are fixed on this branch: a leading UTF-8 BOM dropped the first
//     group (RFC 9309 section 2.1; corpus: bom.txt), and non-ASCII patterns
//     like "Disallow: /café" never matched because Allows() tests the
//     percent-encoded path (corpus: unicode.txt).
func TestDifferentialVsGoogle(t *testing.T) {
	files, err := filepath.Glob("testdata/*.txt")
	if err != nil || len(files) == 0 {
		t.Fatal("testdata corpus missing")
	}
	// A fixed probe set exercising prefix matches, wildcards, $-anchors,
	// query strings, and case sensitivity.
	probePaths := []string{
		"/", "/about", "/private/", "/private/secret",
		"/public/x", "/index.php", "/index.php5",
		"/search", "/searching", "/tmp", "/tmp/x",
		"/up", "/up/ok", "/x", "/d", "/b",
		"/a?q=1", "/a?b=c", "/a+b", "/axb",
		"/c(d)", "/e.f", "/exf", "/caf\u00e9", "/cafe",
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		r := Parse(string(body))
		name := filepath.Base(f)
		for _, p := range probePaths {
			uri := "https://example.com" + p
			u, err := url.Parse(uri)
			if err != nil {
				t.Fatalf("probe URL %q does not parse: %v", uri, err)
			}
			ours := r.Allows(u)
			theirs := gro.AgentAllowed(string(body), "*", uri)
			if ours != theirs {
				t.Errorf("%s %q: ours=%v, google=%v", name, p, ours, theirs)
			}
		}
	}
}
