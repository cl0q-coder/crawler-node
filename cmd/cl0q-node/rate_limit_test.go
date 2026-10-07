package main

// A1: tests for Retry-After honoring (rateGate) and parseRetryAfter.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseRetryAfterSeconds(t *testing.T) {
	if d := parseRetryAfter("120"); d != 120*time.Second {
		t.Fatalf("got %v, want 120s", d)
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	future := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	d := parseRetryAfter(future)
	if d < 85*time.Second || d > 95*time.Second {
		t.Fatalf("got %v, want ~90s", d)
	}
}

func TestParseRetryAfterGarbage(t *testing.T) {
	for _, v := range []string{"", "soon", "-5", "Tue, 01 Jan 1980 00:00:00 GMT"} {
		if d := parseRetryAfter(v); d != 0 {
			t.Fatalf("parseRetryAfter(%q) = %v, want 0", v, d)
		}
	}
}

func TestRateGateNoteAndRemaining(t *testing.T) {
	g := &rateGate{}
	if r := g.remaining(); r > 0 {
		t.Fatalf("fresh gate remaining = %v, want <= 0", r)
	}
	g.note(10 * time.Second)
	r := g.remaining()
	// 10s + up-to-5s jitter, allow a little clock slop.
	if r < 9*time.Second || r > 16*time.Second {
		t.Fatalf("remaining = %v, want ~10-15s", r)
	}
	// A shorter note must not pull the deadline back.
	g.note(time.Second)
	if r2 := g.remaining(); r2 < 9*time.Second {
		t.Fatalf("shorter note shrank deadline: %v", r2)
	}
}

func TestRateGateCapsAtMax(t *testing.T) {
	g := &rateGate{}
	g.note(48 * time.Hour) // quota-style Retry-After
	if r := g.remaining(); r > maxRateBackoff+6*time.Second {
		t.Fatalf("remaining = %v, want capped near %v", r, maxRateBackoff)
	}
}

func TestPostCapturesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	err := post(srv.Client(), srv.URL, "tok", map[string]string{"a": "b"})
	var se *statusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want *statusError", err, err)
	}
	if se.status != 429 {
		t.Fatalf("status = %d, want 429", se.status)
	}
	if se.retryAfter != 30*time.Second {
		t.Fatalf("retryAfter = %v, want 30s", se.retryAfter)
	}
}

func TestPostNoRetryAfterOnOtherStatuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	err := post(srv.Client(), srv.URL, "tok", map[string]string{"a": "b"})
	var se *statusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want *statusError", err, err)
	}
	if se.retryAfter != 0 {
		t.Fatalf("retryAfter = %v on 400, want 0", se.retryAfter)
	}
}
