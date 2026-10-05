package footprint

import (
	"testing"
	"time"
)

// NR3: the cap decision logic must be exact — a node pauses discovery at
// the cap, never before, and a zero max disables the cap entirely.

func TestEvaluateCaps(t *testing.T) {
	// Under every cap: nothing fires.
	s := evaluateCaps(100, 100, 1e9, 1000, 1000, 10<<30)
	if s.domainsCap || s.pagesCap || s.diskCap {
		t.Errorf("under caps: %+v — nothing should fire", s)
	}
	// At exactly the cap: fires (>=, not >).
	s = evaluateCaps(1000, 100, 1e9, 1000, 1000, 10<<30)
	if !s.domainsCap || s.pagesCap || s.diskCap {
		t.Errorf("at domains cap: %+v — only domainsCap should fire", s)
	}
	s = evaluateCaps(100, 1000, 1e9, 1000, 1000, 10<<30)
	if s.domainsCap || !s.pagesCap || s.diskCap {
		t.Errorf("at pages cap: %+v — only pagesCap should fire", s)
	}
	s = evaluateCaps(100, 100, 10<<30, 1000, 1000, 10)
	if s.domainsCap || s.pagesCap || !s.diskCap {
		t.Errorf("at disk cap: %+v — only diskCap should fire", s)
	}
	// Over the cap: fires.
	s = evaluateCaps(5000, 5000, 99<<30, 1000, 1000, 10)
	if !s.domainsCap || !s.pagesCap || !s.diskCap {
		t.Errorf("over all caps: %+v — everything should fire", s)
	}
	// Zero max disables that cap even when the size is huge.
	s = evaluateCaps(1<<60, 1<<60, 1<<60, 0, 0, 0)
	if s.domainsCap || s.pagesCap || s.diskCap {
		t.Errorf("zero maxes: %+v — nothing should fire", s)
	}
	// Sizes are carried through for the log message.
	s = evaluateCaps(1234, 5678, 9e9, 1000, 1000, 10<<30)
	if s.domains != 1234 || s.pages != 5678 || s.dbBytes != 9e9 {
		t.Errorf("sizes not carried: %+v", s)
	}
}

func TestNewReadsEnv(t *testing.T) {
	t.Setenv("NODE_MAX_DOMAINS", "42")
	t.Setenv("NODE_MAX_PAGES", "43")
	t.Setenv("NODE_MAX_DB_GB", "44")
	c := New(nil)
	if c.maxDomains != 42 || c.maxPages != 43 || c.maxDBBytes != 44<<30 {
		t.Errorf("env not read: %+v", c)
	}
	// Defaults when unset.
	t.Setenv("NODE_MAX_DOMAINS", "")
	t.Setenv("NODE_MAX_PAGES", "")
	t.Setenv("NODE_MAX_DB_GB", "")
	// empty string falls back to the default via config.GetInt
	c = New(nil)
	if c.maxDomains != 1000000 || c.maxPages != 1000000 || c.maxDBBytes != 20<<30 {
		t.Errorf("defaults wrong: %+v", c)
	}
}

func TestAtCapLogThrottled(t *testing.T) {
	c := &Checker{}
	m1 := c.atCapLogf("cap %d", 1)
	m2 := c.atCapLogf("cap %d", 1)
	if m1 == "" || m2 == "" {
		t.Error("atCapLogf must always return the message")
	}
	if m1 != m2 {
		t.Errorf("messages differ: %q vs %q", m1, m2)
	}
	// A call after the 10m throttle window still returns the message.
	c.lastLog = time.Now().Add(-11 * time.Minute)
	m3 := c.atCapLogf("cap %d", 2)
	if m3 == "" {
		t.Error("expected message after throttle window")
	}
}
