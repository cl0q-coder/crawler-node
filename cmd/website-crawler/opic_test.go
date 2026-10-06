package main

import (
	"math"
	"testing"
)

func TestOpicSharesBasic(t *testing.T) {
	// A has cash 10, links to B and C. B has cash 4, links to C.
	sources := map[string][]string{
		"a.com": {"b.com", "c.com"},
		"b.com": {"c.com"},
	}
	cash := map[string]float64{"a.com": 10, "b.com": 4}
	got := opicShares(sources, cash)
	// a.com: 5 to b.com, 5 to c.com. b.com: 4 to c.com.
	if got["b.com"] != 5 {
		t.Errorf("b.com share = %v, want 5", got["b.com"])
	}
	if got["c.com"] != 9 {
		t.Errorf("c.com share = %v, want 9", got["c.com"])
	}
	if len(got) != 2 {
		t.Errorf("got %d destinations, want 2", len(got))
	}
}

func TestOpicSharesNoSelfPayment(t *testing.T) {
	sources := map[string][]string{"a.com": {"a.com", "b.com"}}
	cash := map[string]float64{"a.com": 10}
	got := opicShares(sources, cash)
	if _, ok := got["a.com"]; ok {
		t.Error("self-payment should be skipped")
	}
	if got["b.com"] != 5 {
		t.Errorf("b.com share = %v, want 5", got["b.com"])
	}
}

func TestOpicSharesZeroCash(t *testing.T) {
	sources := map[string][]string{"a.com": {"b.com"}}
	cash := map[string]float64{"a.com": 0}
	if got := opicShares(sources, cash); len(got) != 0 {
		t.Errorf("zero cash should distribute nothing, got %v", got)
	}
	// Missing cash entry → 0.
	if got := opicShares(sources, map[string]float64{}); len(got) != 0 {
		t.Errorf("missing cash should distribute nothing, got %v", got)
	}
}

func TestOpicSharesEmpty(t *testing.T) {
	if got := opicShares(map[string][]string{}, map[string]float64{}); len(got) != 0 {
		t.Errorf("empty sources, got %v", got)
	}
	sources := map[string][]string{"a.com": {}}
	cash := map[string]float64{"a.com": 10}
	if got := opicShares(sources, cash); len(got) != 0 {
		t.Errorf("no outlinks, got %v", got)
	}
}

func TestOpicSharesConservation(t *testing.T) {
	// Total cash distributed (excluding self-payments) should equal the sum of
	// source cash for sources with outlinks.
	sources := map[string][]string{
		"a.com": {"b.com", "c.com", "d.com"},
		"b.com": {"c.com"},
		"c.com": {"a.com", "b.com"},
	}
	cash := map[string]float64{"a.com": 12, "b.com": 6, "c.com": 8}
	got := opicShares(sources, cash)
	var total float64
	for _, v := range got {
		total += v
	}
	// a.com distributes 12 (no self links), b.com distributes 6, c.com distributes 8.
	want := 12.0 + 6.0 + 8.0
	if math.Abs(total-want) > 1e-9 {
		t.Errorf("total distributed = %v, want %v (cash conserved)", total, want)
	}
}
