package main

import "testing"

// Fail-open paths of filterSubdomainCap must never touch the DB: with
// idxReady=false (migration pending) or maxSubs<=0 the batch passes through
// unchanged, so a nil *sql.DB is safe here by construction.
func TestFilterSubdomainCapFailOpenNoIndex(t *testing.T) {
	batch := []pendDomain{
		{domain: "a.example.com", registrable: "example.com", isSubdomain: true},
		{domain: "b.example.com", registrable: "example.com", isSubdomain: true},
		{domain: "example.com", registrable: "example.com", isSubdomain: false},
	}
	got := filterSubdomainCap(nil, batch, 10000, false)
	if len(got) != len(batch) {
		t.Fatalf("idxReady=false: got %d entries, want %d (fail open)", len(got), len(batch))
	}
	for i := range got {
		if got[i] != batch[i] {
			t.Fatalf("idxReady=false: entry %d modified, want pass-through", i)
		}
	}
}

func TestFilterSubdomainCapDisabled(t *testing.T) {
	batch := []pendDomain{
		{domain: "a.example.com", registrable: "example.com", isSubdomain: true},
	}
	if got := filterSubdomainCap(nil, batch, 0, true); len(got) != 1 {
		t.Fatalf("maxSubs=0: got %d entries, want 1 (disabled)", len(got))
	}
	if got := filterSubdomainCap(nil, nil, 10000, true); got != nil {
		t.Fatalf("empty batch: got %v, want nil", got)
	}
}
