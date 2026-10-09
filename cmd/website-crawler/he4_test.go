package main

import (
	"math"
	"testing"
)

func TestHe4ChangeObserved304(t *testing.T) {
	// 304 → definitively unchanged, regardless of fingerprints.
	if changed, known := he4ChangeObserved(true, 0xdeadbeef, 0x12345678); !known || changed {
		t.Errorf("304: got changed=%v known=%v, want false,true", changed, known)
	}
}

func TestHe4ChangeObservedSimHash(t *testing.T) {
	base := uint64(0xAAAAAAAAAAAAAAAA)
	// Identical fingerprints → unchanged.
	if changed, known := he4ChangeObserved(false, base, base); !known || changed {
		t.Errorf("identical: got changed=%v known=%v, want false,true", changed, known)
	}
	// 1 bit differs → unchanged (below the 8-bit threshold).
	if changed, known := he4ChangeObserved(false, base, base^0x1); !known || changed {
		t.Errorf("1-bit: got changed=%v known=%v, want false,true", changed, known)
	}
	// 7 bits differ → still unchanged (threshold is >= 8).
	if changed, known := he4ChangeObserved(false, base, base^0x7F); !known || changed {
		t.Errorf("7-bit: got changed=%v known=%v, want false,true", changed, known)
	}
	// 8 bits differ → changed.
	if changed, known := he4ChangeObserved(false, base, base^0xFF); !known || !changed {
		t.Errorf("8-bit: got changed=%v known=%v, want true,true", changed, known)
	}
	// Missing fingerprint → unknown verdict.
	if _, known := he4ChangeObserved(false, base, 0); known {
		t.Error("missing stored fp: want known=false")
	}
	if _, known := he4ChangeObserved(false, 0, base); known {
		t.Error("missing fresh fp: want known=false")
	}
}

func TestHe4NewChangeRate(t *testing.T) {
	// Unset → default 1/120, then one observed change.
	if got := he4NewChangeRate(0, true); math.Abs(got-(he4ChangeRateDefault*0.7+0.3)) > 1e-12 {
		t.Errorf("unset+changed: got %v", got)
	}
	// EWMA step: 0.5, unchanged → 0.35.
	if got := he4NewChangeRate(0.5, false); math.Abs(got-0.35) > 1e-12 {
		t.Errorf("0.5+unchanged: got %v, want 0.35", got)
	}
	// Clamp: the EWMA of constant obs=1 converges to 1.0 (the cap 2.0 is a
	// guard, unreachable via binary observations); repeated no-changes
	// converge to the 1/365 floor, never below.
	v := 0.5
	for i := 0; i < 200; i++ {
		v = he4NewChangeRate(v, true)
	}
	if math.Abs(v-1.0) > 1e-9 {
		t.Errorf("repeated change: got %v, want 1.0", v)
	}
	// Clamp: repeated no-changes converge to the 1/365 floor, never below.
	v = 0.5
	for i := 0; i < 200; i++ {
		v = he4NewChangeRate(v, false)
	}
	if v != he4ChangeRateMin {
		t.Errorf("repeated no-change: got %v, want floor %v", v, he4ChangeRateMin)
	}
	// Convergence: a daily changer's λ converges near 1.0.
	v = he4ChangeRateDefault
	for i := 0; i < 60; i++ {
		v = he4NewChangeRate(v, true)
	}
	if math.Abs(v-1.0) > 0.05 {
		t.Errorf("daily changer λ = %v, want ≈1.0", v)
	}
}

func TestHe4PriorityScore(t *testing.T) {
	// A daily news page outranks a static brochure at equal age/importance.
	news := he4PriorityScore(10, 1.0, 30, 120, false)
	brochure := he4PriorityScore(10, 1.0/365, 30, 120, false)
	if news <= brochure {
		t.Errorf("news %v should outrank brochure %v", news, brochure)
	}
	// Higher importance wins at equal staleness.
	hi := he4PriorityScore(100, 0.1, 30, 120, false)
	lo := he4PriorityScore(1, 0.1, 30, 120, false)
	if hi <= lo {
		t.Errorf("importance: hi %v should exceed lo %v", hi, lo)
	}
	// Overdue (past interval) beats merely old: same age, shorter interval → higher P.
	overdue := he4PriorityScore(10, 0.1, 60, 30, false) // 2× overdue
	notDue := he4PriorityScore(10, 0.1, 60, 120, false) // 0.5× overdue
	if overdue <= notDue {
		t.Errorf("overdue %v should exceed not-due %v", overdue, notDue)
	}
	// Starvation guard: a low-λ page past its (long) interval still scores > 0
	// and grows with age — it will eventually outrank everything due sooner.
	old := he4PriorityScore(1, 1.0/365, 400, 120, false)
	if old <= 0 {
		t.Errorf("deep-overdue low-λ page must score > 0, got %v", old)
	}
	// Sitemap hint doubles the score (pre-fetch promotion).
	base := he4PriorityScore(10, 0.1, 30, 120, false)
	hinted := he4PriorityScore(10, 0.1, 30, 120, true)
	if math.Abs(hinted-2*base) > 1e-12 {
		t.Errorf("sitemap hint: got %v, want 2×%v", hinted, base)
	}
	// Fresh page (age 0) scores 0 — nothing to gain.
	if got := he4PriorityScore(10, 1.0, 0, 120, false); got != 0 {
		t.Errorf("fresh page: got %v, want 0", got)
	}
}

func TestHe4T3Factors(t *testing.T) {
	// λ→0 reproduces the legacy constants (graceful degradation to T3-classic).
	if got := he4TightenFactor(0); math.Abs(got-0.75) > 1e-12 {
		t.Errorf("tighten(0) = %v, want 0.75", got)
	}
	if got := he4BackoffFactor(0); math.Abs(got-1.1) > 1e-12 {
		t.Errorf("backoff(0) = %v, want 1.1", got)
	}
	// Fast changer: tightens toward ×0.5, backs off toward ×1.25.
	if got := he4TightenFactor(2.0); got < 0.5 || got > 0.55 {
		t.Errorf("tighten(2) = %v, want ≈0.5", got)
	}
	if got := he4BackoffFactor(2.0); got < 1.2 || got > 1.25 {
		t.Errorf("backoff(2) = %v, want ≈1.25", got)
	}
	// Monotonic: faster λ → harder tighten, stronger backoff.
	if he4TightenFactor(1.0) >= he4TightenFactor(0.01) {
		t.Error("tighten factor should decrease with λ")
	}
	if he4BackoffFactor(1.0) <= he4BackoffFactor(0.01) {
		t.Error("backoff factor should increase with λ")
	}
}

func TestT3NextIntervalLambda(t *testing.T) {
	// Slow changer (λ→default 1/120), changed: ≈legacy ×0.75.
	if got, want := t3NextIntervalLambda(8, true, 0, 7), 8*he4TightenFactor(he4ChangeRateDefault); math.Abs(got-want) > 1e-9 {
		t.Errorf("slow+changed: got %v, want %v", got, want)
	}
	// Fast changer (λ=2), changed: ×~0.534.
	if got := t3NextIntervalLambda(8, true, 2.0, 7); math.Abs(got-8*he4TightenFactor(2.0)) > 1e-9 {
		t.Errorf("fast+changed: got %v", got)
	}
	// Unset → default first, then backoff — capped at 30.
	if got := t3NextIntervalLambda(0, false, 0, 120); got != 30 {
		t.Errorf("unset: got %v, want cap 30", got)
	}
	// Floors/caps hold.
	if got := t3NextIntervalLambda(1, true, 2.0, 7); got != 1 {
		t.Errorf("floor: got %v, want 1", got)
	}
	if got := t3NextIntervalLambda(30, false, 2.0, 7); got != 30 {
		t.Errorf("cap: got %v, want 30", got)
	}
	// Convergence: a daily changer's interval converges to ≈1/λ = 1 day.
	iv, lambda := 120.0, 1.0
	for i := 0; i < 40; i++ {
		iv = t3NextIntervalLambda(iv, true, lambda, 120)
	}
	if math.Abs(iv-1.0) > 0.2 {
		t.Errorf("daily changer interval = %v, want ≈1", iv)
	}
	// A stable page's interval converges to the 30-day cap.
	iv = 7.0
	for i := 0; i < 40; i++ {
		iv = t3NextIntervalLambda(iv, false, 1.0/365, 120)
	}
	if iv != 30 {
		t.Errorf("stable page interval = %v, want 30", iv)
	}
}

func TestHe4ScoreSQLParity(t *testing.T) {
	// The SQL score expression must match he4PriorityScore on representative
	// inputs. Can't run SQL here (no DB in unit tests) — this pins the Go
	// side; the SQL is reviewed against the same formula (see he4ScoreExpr).
	cases := []struct {
		imp, lambda, age, iv float64
		hint                 bool
	}{
		{10, 1.0, 30, 120, false},
		{1, 1.0 / 365, 400, 30, true},
		{100, 2.0, 1, 1, false},
	}
	for _, c := range cases {
		got := he4PriorityScore(c.imp, c.lambda, c.age, c.iv, c.hint)
		// Recompute the SQL formula term-by-term in Go.
		expGain := 1 - math.Exp(-c.lambda*c.age)
		overdue := c.age / c.iv
		want := c.imp * expGain * overdue
		if c.hint {
			want *= he4SitemapBoost
		}
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("score mismatch: got %v want %v", got, want)
		}
	}
}
