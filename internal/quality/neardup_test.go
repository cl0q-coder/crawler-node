package quality

import (
	"math"
	"testing"
	"time"
)

// Fixture fingerprints. Distances are ASSERTED (computed, not vibes) in
// TestNearDupFixtureArithmetic before any clustering test relies on them.
//
// NOTE: fingerprint 0x0 is reserved ("not computed" — SimHash of empty text
// and the codebase's not-computed sentinel coincide), so every fixture
// fingerprint is nonzero; the zero case is covered by
// TestClusterNearDupSkipsZeroFingerprint.
const (
	fpBase = uint64(0xAAAAAAAAAAAAAAAA)
	fpD3   = fpBase ^ 0x7               // 3 bits flipped
	fpD4   = fpBase ^ 0xF               // 4 bits flipped
	fpFar  = uint64(0x5555555555555555) // every bit flipped vs fpBase
	fpT0   = fpBase                     // transitive chain: T0~T1 (dist 2),
	fpT1   = fpBase ^ 0x3               //   T1~T2 (dist 2), T0!~T2 (dist 4)
	fpT2   = fpBase ^ 0xF
)

func TestNearDupFixtureArithmetic(t *testing.T) {
	cases := []struct {
		name string
		a, b uint64
		want int
	}{
		{"base~d3 is distance 3", fpBase, fpD3, 3},
		{"base~d4 is distance 4", fpBase, fpD4, 4},
		{"base~far is distance 64", fpBase, fpFar, 64},
		{"identical is distance 0", fpBase, fpBase, 0},
		{"transitive chain link T0~T1", fpT0, fpT1, 2},
		{"transitive chain link T1~T2", fpT1, fpT2, 2},
		{"transitive chain ends T0!~T2", fpT0, fpT2, 4},
	}
	for _, c := range cases {
		if got := hammingDistance(c.a, c.b); got != c.want {
			t.Errorf("%s: hammingDistance = %d, want %d", c.name, got, c.want)
		}
	}
}

func k3page(url, domain string, fp uint64, ageDays int, authority float64, parked bool) NearDupPage {
	return NearDupPage{
		URL:       url,
		Domain:    domain,
		SimHash:   fp,
		FirstSeen: time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour),
		Authority: authority,
		Parked:    parked,
	}
}

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// A near-dup pair (distance exactly 3, the default threshold) is reported
// honestly: both members flagged, cluster size 2, oldest crawl wins the
// representative slot, authority = max of the cluster.
func TestClusterNearDupPair(t *testing.T) {
	pages := []NearDupPage{
		k3page("https://new.example/page", "new.example", fpD3, 1, 3.0, false),    // newer, higher authority
		k3page("https://old.example/page", "old.example", fpBase, 30, 1.5, false), // older = original
	}
	vs := ClusterNearDup(pages)
	if len(vs) != 2 {
		t.Fatalf("got %d verdicts, want 2", len(vs))
	}
	for i, v := range vs {
		if !v.IsNearDup {
			t.Errorf("page %d: IsNearDup = false, want true", i)
		}
		if v.ClusterSize != 2 {
			t.Errorf("page %d: ClusterSize = %d, want 2", i, v.ClusterSize)
		}
		if v.RepresentativeURL != "https://old.example/page" {
			t.Errorf("page %d: RepresentativeURL = %q, want the oldest crawl", i, v.RepresentativeURL)
		}
		if v.DistinctDomainCount != 2 {
			t.Errorf("page %d: DistinctDomainCount = %d, want 2", i, v.DistinctDomainCount)
		}
		if len(v.SampleDomains) != 2 || v.SampleDomains[0] != "new.example" || v.SampleDomains[1] != "old.example" {
			t.Errorf("page %d: SampleDomains = %v, want both domains sorted", i, v.SampleDomains)
		}
		if !approxEq(v.AuthorityScore, 3.0) {
			t.Errorf("page %d: AuthorityScore = %v, want cluster max 3.0", i, v.AuthorityScore)
		}
	}
	// Confidence: rep is distance 0 (1.0); member is distance 3 at
	// threshold 3 → 1 - 3/4 = 0.25.
	if !approxEq(vs[1].Confidence, 1.0) {
		t.Errorf("representative Confidence = %v, want 1.0", vs[1].Confidence)
	}
	if !approxEq(vs[0].Confidence, 0.25) {
		t.Errorf("member Confidence = %v, want 0.25", vs[0].Confidence)
	}
}

// Distinct pages (no shared band at the default threshold) come back as
// singletons: not dups, cluster size 1, own URL as representative.
func TestClusterNearDupDistinct(t *testing.T) {
	pages := []NearDupPage{
		k3page("https://a.example/", "a.example", fpBase, 5, 1.0, false),
		k3page("https://b.example/", "b.example", fpFar, 5, 1.0, false),
	}
	vs := ClusterNearDup(pages)
	for i, v := range vs {
		if v.IsNearDup {
			t.Errorf("page %d: IsNearDup = true, want false", i)
		}
		if v.ClusterSize != 1 {
			t.Errorf("page %d: ClusterSize = %d, want 1", i, v.ClusterSize)
		}
		if v.RepresentativeURL != pages[i].URL {
			t.Errorf("page %d: RepresentativeURL = %q, want own URL", i, v.RepresentativeURL)
		}
		if v.Confidence != 0 {
			t.Errorf("page %d: Confidence = %v, want 0", i, v.Confidence)
		}
	}
}

// Threshold boundary: distance 3 merges at threshold 3 but not 2; distance
// 4 merges at threshold 4 but not 3. The banding stays recall-exact when the
// threshold moves (bands = threshold+1), so no silent recall cliff.
func TestClusterNearDupThresholdBoundary(t *testing.T) {
	cases := []struct {
		name      string
		a, b      uint64
		threshold int
		wantDup   bool
	}{
		{"dist3 at threshold 3", fpBase, fpD3, 3, true},
		{"dist3 at threshold 2", fpBase, fpD3, 2, false},
		{"dist4 at threshold 4", fpBase, fpD4, 4, true},
		{"dist4 at threshold 3", fpBase, fpD4, 3, false},
		{"dist0 at threshold 0", fpBase, fpBase, 0, true},
		{"dist3 at threshold 0", fpBase, fpD3, 0, false},
	}
	for _, c := range cases {
		pages := []NearDupPage{
			k3page("https://a.example/", "a.example", c.a, 5, 1.0, false),
			k3page("https://b.example/", "b.example", c.b, 5, 1.0, false),
		}
		vs := ClusterNearDupWithThreshold(pages, c.threshold)
		if vs[0].IsNearDup != c.wantDup || vs[1].IsNearDup != c.wantDup {
			t.Errorf("%s: IsNearDup = (%v,%v), want %v",
				c.name, vs[0].IsNearDup, vs[1].IsNearDup, c.wantDup)
		}
	}
}

// Union-find transitivity: A~B and B~C merge even though A and C are
// distance 4 apart (beyond the threshold). A pairwise-only clusterer would
// split them.
func TestClusterNearDupTransitive(t *testing.T) {
	pages := []NearDupPage{
		k3page("https://t0.example/", "t0.example", fpT0, 30, 1.0, false),
		k3page("https://t1.example/", "t1.example", fpT1, 20, 1.0, false),
		k3page("https://t2.example/", "t2.example", fpT2, 10, 1.0, false),
	}
	vs := ClusterNearDup(pages)
	for i, v := range vs {
		if !v.IsNearDup || v.ClusterSize != 3 {
			t.Errorf("page %d: IsNearDup=%v ClusterSize=%d, want true/3", i, v.IsNearDup, v.ClusterSize)
		}
		if v.RepresentativeURL != "https://t0.example/" {
			t.Errorf("page %d: RepresentativeURL = %q, want oldest", i, v.RepresentativeURL)
		}
	}
}

// Mirror-farm shape: 6 pages, 6 distinct domains, 4 of them K1-parked.
// The verdict must carry the disambiguation signals: full domain count,
// capped sorted sample, and the parked cross-ref — NOT a binary farm call.
func TestClusterNearDupFarm(t *testing.T) {
	base := uint64(0xF00DF00DF00DF00D)
	flips := []uint64{0x0, 0x1, 0x2, 0x3, 0x4, 0x6}
	pages := make([]NearDupPage, 6)
	for i := range pages {
		d := [6]string{"farm1.example", "farm2.example", "farm3.example", "farm4.example", "farm5.example", "farm6.example"}[i]
		parked := i < 4 // 4 of the 6 are K1-parked
		pages[i] = k3page("https://"+d+"/", d, base^flips[i], 10-i, 1.0, parked)
	}
	// Oldest crawl is farm3 (index 2) — it must win the representative slot
	// even though it is not first in input order.
	pages[2].FirstSeen = time.Now().Add(-90 * 24 * time.Hour)

	vs := ClusterNearDup(pages)
	for i, v := range vs {
		if !v.IsNearDup || v.ClusterSize != 6 {
			t.Fatalf("page %d: IsNearDup=%v ClusterSize=%d, want true/6", i, v.IsNearDup, v.ClusterSize)
		}
		if v.RepresentativeURL != "https://farm3.example/" {
			t.Errorf("page %d: RepresentativeURL = %q, want oldest (farm3)", i, v.RepresentativeURL)
		}
		if v.DistinctDomainCount != 6 {
			t.Errorf("page %d: DistinctDomainCount = %d, want 6", i, v.DistinctDomainCount)
		}
		if len(v.SampleDomains) != maxSampleDomains {
			t.Errorf("page %d: len(SampleDomains) = %d, want cap %d", i, len(v.SampleDomains), maxSampleDomains)
		}
		for j := 1; j < len(v.SampleDomains); j++ {
			if v.SampleDomains[j-1] >= v.SampleDomains[j] {
				t.Errorf("page %d: SampleDomains %v not sorted", i, v.SampleDomains)
			}
		}
		if v.ParkedMembers != 4 {
			t.Errorf("page %d: ParkedMembers = %d, want 4 (K1 cross-ref)", i, v.ParkedMembers)
		}
	}
}

// Legit-mirror shape: 2 domains, identical content, no parked signal.
// Reported as a 2-member cluster (Micah: no suppression in the detector) —
// the wire-in decides, using the 2-domain + 0-parked signals, that this is
// a legit mirror, not a farm.
func TestClusterNearDupLegitMirror(t *testing.T) {
	pages := []NearDupPage{
		k3page("https://mirror.example.net/", "example.net", fpBase, 2, 2.0, false),
		k3page("https://www.example.com/", "example.com", fpBase, 400, 5.0, false),
	}
	vs := ClusterNearDup(pages)
	for i, v := range vs {
		if !v.IsNearDup || v.ClusterSize != 2 {
			t.Errorf("page %d: IsNearDup=%v ClusterSize=%d, want true/2", i, v.IsNearDup, v.ClusterSize)
		}
		if v.RepresentativeURL != "https://www.example.com/" {
			t.Errorf("page %d: RepresentativeURL = %q, want oldest", i, v.RepresentativeURL)
		}
		if v.DistinctDomainCount != 2 || v.ParkedMembers != 0 {
			t.Errorf("page %d: domains=%d parked=%d, want 2/0", i, v.DistinctDomainCount, v.ParkedMembers)
		}
		if !approxEq(v.Confidence, 1.0) {
			t.Errorf("page %d: Confidence = %v, want 1.0 (identical)", i, v.Confidence)
		}
		if !approxEq(v.AuthorityScore, 5.0) {
			t.Errorf("page %d: AuthorityScore = %v, want 5.0", i, v.AuthorityScore)
		}
	}
}

// SimHash == 0 means "fingerprint not computed": the page is skipped, never
// clustered, even next to an identical-looking cluster.
func TestClusterNearDupSkipsZeroFingerprint(t *testing.T) {
	pages := []NearDupPage{
		k3page("https://a.example/", "a.example", fpBase, 5, 1.0, false),
		k3page("https://b.example/", "b.example", fpBase, 5, 1.0, false),
		k3page("https://c.example/", "c.example", 0, 5, 1.0, false),
	}
	vs := ClusterNearDup(pages)
	if !vs[0].IsNearDup || !vs[1].IsNearDup || vs[0].ClusterSize != 2 {
		t.Errorf("fingerprinted pair: want a 2-cluster, got %+v / %+v", vs[0], vs[1])
	}
	if vs[2].IsNearDup || vs[2].ClusterSize != 1 {
		t.Errorf("zero-fingerprint page: want singleton, got %+v", vs[2])
	}
}

// Representative tie-break: equal FirstSeen → lexicographically smallest
// URL, so the choice is stable across batch orderings.
func TestClusterNearDupRepresentativeTieBreak(t *testing.T) {
	ts := time.Now().Add(-24 * time.Hour)
	mk := func(url, domain string) NearDupPage {
		return NearDupPage{URL: url, Domain: domain, SimHash: fpBase, FirstSeen: ts, Authority: 1.0}
	}
	pages := []NearDupPage{
		mk("https://zzz.example/", "zzz.example"),
		mk("https://aaa.example/", "aaa.example"),
		mk("https://mmm.example/", "mmm.example"),
	}
	vs := ClusterNearDup(pages)
	for i, v := range vs {
		if v.RepresentativeURL != "https://aaa.example/" {
			t.Errorf("page %d: RepresentativeURL = %q, want smallest URL", i, v.RepresentativeURL)
		}
	}
}

// Banding is recall-exact: any pair within the threshold shares a band.
// (Deterministic sweep over all distances 0..3 from a base fingerprint,
// plus the negative control that a far fingerprint shares none.)
func TestNearDupBandingExactness(t *testing.T) {
	shareBand := func(a, b []uint64) bool {
		for i := range a {
			if a[i] == b[i] {
				return true
			}
		}
		return false
	}
	ba := bands(fpBase, NearDupHammingThreshold)
	for d := 0; d <= NearDupHammingThreshold; d++ {
		other := fpBase ^ ((uint64(1) << uint(d)) - 1)
		if !shareBand(ba, bands(other, NearDupHammingThreshold)) {
			t.Errorf("distance-%d pair shares no band: recall broken", d)
		}
	}
	if shareBand(ba, bands(fpFar, NearDupHammingThreshold)) {
		t.Errorf("far fingerprint shares a band with base: expected none")
	}
	if n := len(bands(fpBase, 5)); n != 6 {
		t.Errorf("bands(threshold=5) = %d bands, want 6 (threshold+1)", n)
	}
}

// Empty input → empty output; no panics on degenerate input.
func TestClusterNearDupEmpty(t *testing.T) {
	if vs := ClusterNearDup(nil); len(vs) != 0 {
		t.Errorf("nil input: got %d verdicts, want 0", len(vs))
	}
	if vs := ClusterNearDupWithThreshold(nil, -1); len(vs) != 0 {
		t.Errorf("negative threshold on nil: got %d verdicts, want 0", len(vs))
	}
	solo := []NearDupPage{k3page("https://solo.example/", "solo.example", fpBase, 1, 1.0, false)}
	if vs := ClusterNearDup(solo); len(vs) != 1 || vs[0].IsNearDup {
		t.Errorf("single page: want one singleton verdict, got %+v", vs)
	}
}
