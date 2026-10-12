package he1

import (
	"testing"
	"time"
)

// ShardOf must be stable and group by registrable domain.
func TestShardOf(t *testing.T) {
	if ShardOf("blog.acme.com", 1024) != ShardOf("acme.com", 1024) {
		t.Error("blog.acme.com and acme.com must share a shard (registrable-domain grouping)")
	}
	if a, b := ShardOf("example.com", 1024), ShardOf("example.com", 1024); a != b {
		t.Error("ShardOf must be deterministic")
	}
	if ShardOf("example.com", 1024) == ShardOf("other.org", 1024) &&
		ShardOf("example.com", 1024) == ShardOf("third.net", 1024) {
		t.Error("distinct domains should not all collide (sanity)")
	}
	// IP literals / public suffixes: stable shard, not a degenerate 0-bucket.
	ip := ShardOf("192.0.2.1", 1024)
	if ip != ShardOf("192.0.2.1", 1024) {
		t.Error("IP shard must be stable")
	}
	if ShardOf("", 1024) != ShardOf("   ", 1024) {
		t.Error("degenerate hosts must hash consistently")
	}
	// Shard ids stay in range.
	for _, d := range []string{"a.com", "b.org", "192.0.2.1", "com"} {
		if s := ShardOf(d, 16); s >= 16 {
			t.Errorf("ShardOf(%q, 16) = %d, out of range", d, s)
		}
	}
}

// Rough uniformity: 20k synthetic domains over 1024 shards should land
// within 2x of the mean everywhere (FNV-1a is not cryptographic, but the
// ring only needs absence of pathological skew).
func TestShardOfDistribution(t *testing.T) {
	const n, shards = 20000, 1024
	counts := make([]int, shards)
	for i := 0; i < n; i++ {
		counts[ShardOf(domainFor(i), shards)]++
	}
	mean := float64(n) / shards
	for s, c := range counts {
		if float64(c) > 2*mean || float64(c) < mean/2 {
			t.Fatalf("shard %d got %d domains, mean %.1f — skewed", s, c, mean)
		}
	}
}

// LeaseWindow: 2× p99, floored 5min, capped 30min, 10min default pre-p99.
func TestLeaseWindow(t *testing.T) {
	cases := []struct {
		p99  time.Duration
		want time.Duration
	}{
		{0, 10 * time.Minute},                // unknown: default
		{-time.Second, 10 * time.Minute},     // nonsense: default
		{time.Minute, 5 * time.Minute},       // 2× = 2min → floor
		{4 * time.Minute, 8 * time.Minute},   // 2× = 8min, in band
		{20 * time.Minute, 30 * time.Minute}, // 2× = 40min → cap
		{time.Hour, 30 * time.Minute},        // cap
	}
	for _, tc := range cases {
		if got := LeaseWindow(tc.p99); got != tc.want {
			t.Errorf("LeaseWindow(%v) = %v, want %v", tc.p99, got, tc.want)
		}
	}
}

// ApplyPushedFloors: stricter-than-local applies; the adopted central value
// only ever moves gentler (one-directional); unset fields are ignored;
// heartbeat interval is authoritative.
func TestApplyPushedFloors(t *testing.T) {
	local := Floors{
		MinHostGap:           2 * time.Second,
		MaxPagesPerHostCycle: 25,
		RecrawlDays:          7,
		HeartbeatInterval:    30 * time.Second,
	}
	var st floorState

	// 1. Central pushes stricter floors → adopted.
	eff := ApplyPushedFloors(local, PushedConfig{
		MinHostGapMs: 5000, MaxPagesPerHostCycle: 10, RecrawlDays: 30, HeartbeatIntervalS: 45,
	}, &st)
	if eff.MinHostGap != 5*time.Second || eff.MaxPagesPerHostCycle != 10 || eff.RecrawlDays != 30 {
		t.Errorf("stricter push not adopted: %+v", eff)
	}
	if eff.HeartbeatInterval != 45*time.Second {
		t.Errorf("heartbeat interval must be authoritative from push, got %v", eff.HeartbeatInterval)
	}

	// 2. Central relaxes (gentler) → adopted relaxes, but effective never
	//    passes local (server can't push a node past its own floors).
	eff = ApplyPushedFloors(local, PushedConfig{
		MinHostGapMs: 1000, MaxPagesPerHostCycle: 50, RecrawlDays: 3,
	}, &st)
	if eff.MinHostGap != 2*time.Second {
		t.Errorf("gap relaxed past local: %v", eff.MinHostGap)
	}
	if eff.MaxPagesPerHostCycle != 25 {
		t.Errorf("pages relaxed past local: %d", eff.MaxPagesPerHostCycle)
	}
	if eff.RecrawlDays != 7 {
		t.Errorf("recrawl relaxed past local: %d", eff.RecrawlDays)
	}

	// 3. Central tries to re-tighten past the relaxation → ignored
	//    (one-directional: adopted can only get gentler).
	eff = ApplyPushedFloors(local, PushedConfig{MinHostGapMs: 8000}, &st)
	if eff.MinHostGap != 2*time.Second {
		t.Errorf("re-tighten must be ignored, effective gap = %v", eff.MinHostGap)
	}

	// 4. Empty push → no change at all.
	before := eff
	eff = ApplyPushedFloors(local, PushedConfig{}, &st)
	if eff != before {
		t.Errorf("empty push changed floors: %+v → %+v", before, eff)
	}
}

// The server can never make the node MORE aggressive than its local floors,
// no matter the push sequence.
func TestFloorsNeverPassLocal(t *testing.T) {
	local := Floors{MinHostGap: 3 * time.Second, MaxPagesPerHostCycle: 20, RecrawlDays: 14, HeartbeatInterval: 30 * time.Second}
	var st floorState
	pushes := []PushedConfig{
		{MinHostGapMs: 500, MaxPagesPerHostCycle: 200, RecrawlDays: 1},
		{MinHostGapMs: 10000, MaxPagesPerHostCycle: 1, RecrawlDays: 90},
		{MinHostGapMs: 100, MaxPagesPerHostCycle: 1000, RecrawlDays: 1},
	}
	for _, p := range pushes {
		eff := ApplyPushedFloors(local, p, &st)
		if eff.MinHostGap < local.MinHostGap {
			t.Errorf("gap %v under local %v", eff.MinHostGap, local.MinHostGap)
		}
		if eff.MaxPagesPerHostCycle > local.MaxPagesPerHostCycle {
			t.Errorf("pages %d over local %d", eff.MaxPagesPerHostCycle, local.MaxPagesPerHostCycle)
		}
		if eff.RecrawlDays < local.RecrawlDays {
			t.Errorf("recrawl %d under local %d", eff.RecrawlDays, local.RecrawlDays)
		}
	}
}
