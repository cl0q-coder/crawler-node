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
