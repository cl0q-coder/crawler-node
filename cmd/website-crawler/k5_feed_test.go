package main

// K5 wire-in tests: feed domains merge into the existing matcher as
// domain-kind rules (exact apex + subdomains), never as keywords, with feed
// provenance on the hit. No feeds configured = matcher behaves as before.

import (
	"testing"
)

func TestBlFeedHit(t *testing.T) {
	mergeFeedBlacklist(map[string]string{"evil.example": "urlhaus"})
	defer mergeFeedBlacklist(nil)
	for h, want := range map[string]bool{
		"evil.example": true, "www.evil.example": true, "a.b.evil.example": true,
		"notevil.example": false, "evil.example.org": false, "example": false,
	} {
		if _, got := blFeedHit(h); got != want {
			t.Errorf("blFeedHit(%q) = %v, want %v", h, got, want)
		}
	}
	if prov, _ := blFeedHit("a.evil.example"); prov != "urlhaus" {
		t.Errorf("provenance = %q, want urlhaus", prov)
	}
}

func TestFeedHitCarriesProvenance(t *testing.T) {
	// Admin list empty; only the feed is installed.
	blMu.Lock()
	oldD, oldW := blDomains, blWords
	blDomains, blWords = nil, nil
	blMu.Unlock()
	defer func() { blMu.Lock(); blDomains, blWords = oldD, oldW; blMu.Unlock() }()
	mergeFeedBlacklist(map[string]string{"phish.example": "openphish"})
	defer mergeFeedBlacklist(nil)

	info := &WebsiteInfo{Title: "totally legit page", Keywords: "", Description: ""}
	term, bad := isBlacklisted("sub.phish.example", info)
	if !bad {
		t.Fatalf("feed-listed subdomain must be blocked")
	}
	if term != "feed:openphish" {
		t.Errorf("term = %q, want feed:openphish (provenance for precision measurement)", term)
	}
	// A legit neighbor must NOT match (no keyword-style eating).
	if term, bad := isBlacklisted("phish.example.org", info); bad {
		t.Errorf("neighbor domain must not match feed rule, got term %q", term)
	}
}

func TestNoFeedsConfiguredIsNoop(t *testing.T) {
	t.Setenv("BLACKLIST_FEEDS", "")
	r := feedRefresherFromEnv()
	if r.Count() != 0 {
		t.Fatalf("empty BLACKLIST_FEEDS must install zero domains, got %d", r.Count())
	}
	// Malformed entries are skipped, not fatal.
	t.Setenv("BLACKLIST_FEEDS", "nourl,also-bad|,ok|https://example.com/feed.txt")
	r = feedRefresherFromEnv()
	if got := len(r.FeedStatus()); got != 1 {
		t.Fatalf("want 1 configured feed after skipping malformed, got %d", got)
	}
}

func TestFeedEnvIntervalBounds(t *testing.T) {
	t.Setenv("BLACKLIST_FEEDS", "")
	t.Setenv("BLACKLIST_FEED_REFRESH", "10s") // below 1m minimum -> 6h default
	_ = feedRefresherFromEnv()                // must not panic; interval clamped inside
	t.Setenv("BLACKLIST_FEED_REFRESH", "not-a-duration")
	_ = feedRefresherFromEnv()
}
