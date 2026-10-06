package config

import (
	"strings"
	"testing"
	"time"
)

func TestValidateIntRange(t *testing.T) {
	// Valid boundaries pass.
	for _, v := range []int{1, 16, 64} {
		if err := ValidateIntRange("WORKERS", v, 1, 64); err != nil {
			t.Errorf("WORKERS=%d should be valid: %v", v, err)
		}
	}
	// Bad values fail with a CLEAR message naming the knob, the value,
	// and the allowed range — this is what the operator sees at startup.
	for _, v := range []int{-5, 0, 65, 1000000} {
		err := ValidateIntRange("WORKERS", v, 1, 64)
		if err == nil {
			t.Errorf("WORKERS=%d should be invalid", v)
			continue
		}
		msg := err.Error()
		for _, want := range []string{"WORKERS", "must be between 1 and 64"} {
			if !strings.Contains(msg, want) {
				t.Errorf("WORKERS=%d error %q should mention %q", v, msg, want)
			}
		}
	}
}

func TestValidateDurationRange(t *testing.T) {
	if err := ValidateDurationRange("HOST_DELAY_MS", 1500*time.Millisecond, time.Second, time.Hour); err != nil {
		t.Errorf("valid duration rejected: %v", err)
	}
	// Below the politeness floor must fail, not silently clamp.
	err := ValidateDurationRange("HOST_DELAY_MS", 100*time.Millisecond, time.Second, time.Hour)
	if err == nil {
		t.Fatal("HOST_DELAY_MS=100ms should be invalid (below 1s politeness floor)")
	}
	if !strings.Contains(err.Error(), "HOST_DELAY_MS") {
		t.Errorf("error should name the knob: %q", err)
	}
}

func TestValidateNonEmpty(t *testing.T) {
	if err := ValidateNonEmpty("INGEST_URL", "https://cl0q.com"); err != nil {
		t.Errorf("valid value rejected: %v", err)
	}
	if err := ValidateNonEmpty("INGEST_URL", ""); err == nil {
		t.Error("empty INGEST_URL should be invalid")
	}
}

func TestPolitenessFloorsCannotBeLowered(t *testing.T) {
	// NR2's core guarantee: the politeness floors from the crawler's
	// promise to site owners (https://cl0q.com/bot) are hard minimums.
	// A setting that would make the crawler FASTER than the floor must
	// fail validation here (call sites enforce the same bounds).
	floors := []struct {
		name string
		v    time.Duration
	}{
		{"HOST_DELAY_MS", 999 * time.Millisecond},
		{"IP_DELAY_MS", 500 * time.Millisecond},
	}
	for _, f := range floors {
		if err := ValidateDurationRange(f.name, f.v, time.Second, time.Hour); err == nil {
			t.Errorf("%s=%s below the 1s politeness floor must be invalid", f.name, f.v)
		}
	}
}

func TestPublicUserAgentForTruncatesToPrefix(t *testing.T) {
	// A long node id is truncated to its first 8 chars in the UA —
	// the full id must not leak to site operators (PROTOCOL.md §4).
	ua, err := PublicUserAgentFor("nd-7f3a9c2b4d1e")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ua, "operator=nd-7f3a9") {
		t.Errorf("UA should carry the 8-char prefix, got %q", ua)
	}
	if strings.Contains(ua, "nd-7f3a9c2b4d1e") {
		t.Errorf("UA must not contain the full node id, got %q", ua)
	}
}

func TestPublicUserAgentForShortIDUnchanged(t *testing.T) {
	// An id at or under 8 chars is used as-is (still a valid botid).
	ua, err := PublicUserAgentFor("nd-1234")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ua, "operator=nd-1234") {
		t.Errorf("short id should be unchanged, got %q", ua)
	}
}

func TestPublicUserAgentForRejectsBadInput(t *testing.T) {
	// Note: underscores are valid — botid allows them (server mints nd_-prefixed ids).
	for _, bad := range []string{"", "x", "ND-UPPER", "nd with space"} {
		if _, err := PublicUserAgentFor(bad); err == nil {
			t.Errorf("PublicUserAgentFor(%q) should fail", bad)
		}
	}
}
