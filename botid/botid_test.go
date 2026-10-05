package botid

import (
	"strings"
	"testing"
)

func TestUserAgent(t *testing.T) {
	ua, err := UserAgent("alice-01")
	if err != nil {
		t.Fatal(err)
	}
	if ua != "Mozilla/5.0 (compatible; cl0q-node/1.0; operator=alice-01; +https://cl0q.com/bot)" {
		t.Fatalf("unexpected UA %q", ua)
	}
	for _, w := range []string{"grabber", "scanner"} { // words WAFs treat as attack tools
		if strings.Contains(strings.ToLower(ua), w) {
			t.Fatalf("UA must not contain %q", w)
		}
	}
}

func TestUserAgentRejectsBadIDs(t *testing.T) {
	for _, id := range []string{"", "a", "Alice", "has space", "x;y", "a)b", "-lead", "_lead", strings.Repeat("a", 33)} {
		if _, err := UserAgent(id); err == nil {
			t.Errorf("node id %q should be rejected", id)
		}
	}
}

func TestUserAgentAcceptsServerIDs(t *testing.T) {
	// The live server mints nd_-prefixed ids (see PROTOCOL.md); the client must accept them.
	for _, id := range []string{"nd_d84cd67b", "nd_7f3a9c", "alice-01", "a_b-c1"} {
		if _, err := UserAgent(id); err != nil {
			t.Errorf("node id %q should be accepted: %v", id, err)
		}
	}
}

func TestIsOurs(t *testing.T) {
	for _, v := range []string{"cl0q-node", "CL0Q-Node", " cl0q ", "cl0q-crawler", "cl0q-discovery", "cl0q-favicon", "cl0q-favicon-grabber"} {
		if !IsOurs(v) {
			t.Errorf("%q should address us", v)
		}
	}
	for _, v := range []string{"*", "googlebot", "cl0q-favicon-x", "cl0qbot", ""} {
		if IsOurs(v) {
			t.Errorf("%q should not address us", v)
		}
	}
}
