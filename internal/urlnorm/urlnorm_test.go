package urlnorm

import (
	"testing"
)

// IG1: normalization must collapse URL variants that fetch the same resource.

func TestNormalizeBasic(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://example.com/", "https://example.com/"},
		{"https://example.com", "https://example.com/"},
		{"HTTPS://EXAMPLE.COM/", "https://example.com/"},
		{"https://example.com:443/", "https://example.com/"},
		{"http://example.com:80/", "http://example.com/"},
		{"https://example.com:8443/", "https://example.com:8443/"},
		{"https://example.com/page#section", "https://example.com/page"},
		{"https://example.com/page?b=2&a=1", "https://example.com/page?a=1&b=2"},
		{"https://example.com/?utm_source=google&q=test", "https://example.com/?q=test"},
		{"https://example.com/?PHPSESSID=abc123&page=1", "https://example.com/?page=1"},
		{"https://user:pass@example.com/", "https://example.com/"},
		{"example.com", "https://example.com/"},
	}
	for _, c := range cases {
		got, err := Normalize(c.in)
		if err != nil {
			t.Errorf("Normalize(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeStripsFragment(t *testing.T) {
	a, _ := Normalize("https://example.com/article#comments")
	b, _ := Normalize("https://example.com/article")
	if a != b {
		t.Errorf("fragments not stripped: %q vs %q", a, b)
	}
}

func TestNormalizeSortsQuery(t *testing.T) {
	a, _ := Normalize("https://example.com/?z=1&a=2&m=3")
	b, _ := Normalize("https://example.com/?a=2&m=3&z=1")
	if a != b {
		t.Errorf("query not sorted: %q vs %q", a, b)
	}
}

func TestNormalizeInvalid(t *testing.T) {
	for _, in := range []string{"", "://bad", "not a url with spaces"} {
		if _, err := Normalize(in); err == nil {
			t.Errorf("Normalize(%q) should error", in)
		}
	}
}

func TestDomain(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://example.com/page", "example.com"},
		{"https://www.example.com/", "example.com"},
		{"HTTPS://WWW.EXAMPLE.COM:443/a?b=c#d", "example.com"},
		{"not-a-url", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := Domain(c.in); got != c.want {
			t.Errorf("Domain(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
