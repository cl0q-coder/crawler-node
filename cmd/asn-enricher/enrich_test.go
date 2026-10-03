package main

import (
	"slices"
	"testing"
)

// Domains whose IP lookup failed stay in the frontier; everything else in the batch is stamped.
func TestWithoutFailed(t *testing.T) {
	ids := []int64{1, 2, 3, 4, 5}
	byIP := map[string][]int64{"1.1.1.1": {1, 2}, "8.8.8.8": {3}, "9.9.9.9": {4}} // id 5 had no IP
	got := withoutFailed(ids, byIP, map[string]bool{"1.1.1.1": true})
	if !slices.Equal(got, []int64{3, 4, 5}) {
		t.Fatalf("got %v, want [3 4 5]", got)
	}
	if got := withoutFailed(ids, byIP, nil); !slices.Equal(got, ids) {
		t.Fatalf("no failures must stamp every id, got %v", got)
	}
}
