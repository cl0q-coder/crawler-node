package crawlutil

import (
	"errors"
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
