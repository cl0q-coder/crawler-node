package main

import (
	"errors"
	"strings"
	"testing"

	"codeberg.org/cl0qsearch/crawler-node/netguard"
)

// TestGuardedClientRefusesNonPublic asserts the preflight HTTP client carries
// the netguard SSRF guard: dials to loopback and RFC 1918 addresses must be
// refused at dial time, before any connection is attempted.
func TestGuardedClientRefusesNonPublic(t *testing.T) {
	c := guardedClient()
	for _, url := range []string{
		"http://127.0.0.1:9/",
		"http://10.0.0.1:80/",
		"http://192.168.1.1:80/",
		"http://169.254.169.254/latest/meta-data/",
	} {
		resp, err := c.Get(url)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("GET %s: expected dial refusal, got a response", url)
		}
		if !errors.Is(err, netguard.ErrNotPublic) && !strings.Contains(err.Error(), "not public") {
			t.Fatalf("GET %s: expected netguard refusal, got: %v", url, err)
		}
	}
}
