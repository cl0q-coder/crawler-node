package main

import (
	"math"
	"testing"
)

func TestT3NextIntervalChanged(t *testing.T) {
	// Changed → tighten ×0.75.
	if got := t3NextInterval(8, true, 7); math.Abs(got-6) > 1e-9 {
		t.Errorf("got %v, want 6", got)
	}
	// Floor at 1 day.
	if got := t3NextInterval(1, true, 7); got != 1 {
		t.Errorf("got %v, want floor 1", got)
	}
	if got := t3NextInterval(1.2, true, 7); got != 1 {
		t.Errorf("got %v, want floor 1", got)
	}
}

func TestT3NextIntervalUnchanged(t *testing.T) {
	// Unchanged → back off ×1.25.
	if got := t3NextInterval(8, false, 7); math.Abs(got-10) > 1e-9 {
		t.Errorf("got %v, want 10", got)
	}
	// Cap at 30 days.
	if got := t3NextInterval(30, false, 7); got != 30 {
		t.Errorf("got %v, want cap 30", got)
	}
	if got := t3NextInterval(25, false, 7); got != 30 {
		t.Errorf("got %v, want cap 30", got)
	}
}

func TestT3NextIntervalUnset(t *testing.T) {
	// Unset (0) → global default first.
	if got := t3NextInterval(0, false, 7); math.Abs(got-8.75) > 1e-9 {
		t.Errorf("got %v, want 8.75", got)
	}
	if got := t3NextInterval(-1, true, 7); math.Abs(got-5.25) > 1e-9 {
		t.Errorf("got %v, want 5.25", got)
	}
}

func TestT3Convergence(t *testing.T) {
	// A stable page: repeated 304s should converge to the 30-day cap.
	iv := 7.0
	for i := 0; i < 20; i++ {
		iv = t3NextInterval(iv, false, 7)
	}
	if iv != 30 {
		t.Errorf("stable page interval = %v, want 30 (cap)", iv)
	}
	// A changing page: repeated changes should converge to the 1-day floor.
	iv = 7.0
	for i := 0; i < 20; i++ {
		iv = t3NextInterval(iv, true, 7)
	}
	if iv != 1 {
		t.Errorf("changing page interval = %v, want 1 (floor)", iv)
	}
}
