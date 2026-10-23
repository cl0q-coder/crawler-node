// Package he1 implements the HE-1 (issue #135) coordination client for the
// agreed PROTOCOL v2 wire: shard-based distributed crawl coordination with a
// central lease service.
//
// Per Claude's build order (2026-10-12), the SIMULATION harness comes first:
// sim.go fakes the central side (ring, lease table, dead detection) so the
// client semantics are proven before any prod wire lands. The central side
// itself (node_leases table, real ring, admin gap §8) is Claude's lane — not
// this package.
//
// Wire summary (PROTOCOL.md §8):
//   - Shards are uint16 ids in a fixed shard space. A domain belongs to
//     exactly one shard: ShardOf hashes its registrable domain.
//   - The central is the SOLE writer of lease state. A node claims domains
//     only from shards it currently holds (OwnsShard) and only draws while
//     its membership epoch matches the last heartbeat.
//   - Config floors pushed by the central are one-directional: the node
//     applies them only if stricter than its local floors, and once a floor
//     is relaxed it never re-tightens (see ApplyPushedFloors).
package he1

import (
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"codeberg.org/cl0qsearch/crawler-node/crawlutil"
)

// ProtocolVersion is the wire version this client speaks. Nodes advertise it
// in enroll + heartbeat; v1 nodes keep working standalone (additive wire).
const ProtocolVersion = 2

// DefaultShardCount is the default shard-space size. Shard ids are uint16;
// 1024 gives ~1k independently assignable buckets with headroom to grow.
const DefaultShardCount = 1024

// ShardOf maps a domain to its shard: FNV-1a over the registrable domain
// (blog.acme.com and acme.com land together, so per-host politeness still
// works), modulo the shard-space size. Hosts with no registrable domain
// (IP literals, public suffixes) hash on the normalized host itself so they
// still get a stable shard instead of colliding on shard 0.
func ShardOf(domain string, shardCount int) uint16 {
	if shardCount <= 0 {
		shardCount = DefaultShardCount
	}
	key := crawlutil.Registrable(domain)
	if key == "" {
		key = strings.ToLower(strings.TrimSpace(domain))
	}
	h := fnv.New64a()
	h.Write([]byte(key))
	return uint16(h.Sum64() % uint64(shardCount))
}

// ShardSet is the set of shards currently assigned to a node.
type ShardSet struct {
	mu sync.RWMutex
	m  map[uint16]struct{}
}

// NewShardSet builds a set from the central's shards_assigned list.
func NewShardSet(shards []uint16) *ShardSet {
	s := &ShardSet{m: make(map[uint16]struct{}, len(shards))}
	for _, sh := range shards {
		s.m[sh] = struct{}{}
	}
	return s
}

// Contains reports whether the node currently holds the shard.
func (s *ShardSet) Contains(shard uint16) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.m[shard]
	return ok
}

// Replace swaps the held set (on heartbeat with a new membership epoch).
func (s *ShardSet) Replace(shards []uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[uint16]struct{}, len(shards))
	for _, sh := range shards {
		m[sh] = struct{}{}
	}
	s.m = m
}

// List returns the held shards in no particular order.
func (s *ShardSet) List() []uint16 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]uint16, 0, len(s.m))
	for sh := range s.m {
		out = append(out, sh)
	}
	return out
}

// Floors are the politeness bounds a node enforces. All four are pushed by
// the central in heartbeat.config (PROTOCOL.md §8); heartbeat_interval is
// special-cased in ApplyPushedFloors because the node must match the
// server's dead-detection cadence to stay alive.
type Floors struct {
	MinHostGap           time.Duration
	MaxPagesPerHostCycle int
	RecrawlDays          int
	HeartbeatInterval    time.Duration
}

// floorState tracks the central-adopted floors across pushes. The adopted
// values only ever move toward GENTLER (the server can relax fleet floors,
// never ratchet them tighter); the effective floor is always the stricter
// of local and adopted, so the server can never push a node past its own
// operator-set politeness.
type floorState struct {
	adopted    Floors
	hasAdopted bool
}

// gentler returns the gentler of two pushed values per dimension: smaller
// gap, larger per-cycle page budget, shorter recrawl window. Zero values in
// b mean "unset" and leave a's dimension alone.
func gentler(a, b Floors) Floors {
	out := a
	if b.MinHostGap > 0 && b.MinHostGap < out.MinHostGap {
		out.MinHostGap = b.MinHostGap
	}
	if b.MaxPagesPerHostCycle > 0 && b.MaxPagesPerHostCycle > out.MaxPagesPerHostCycle {
		out.MaxPagesPerHostCycle = b.MaxPagesPerHostCycle
	}
	if b.RecrawlDays > 0 && b.RecrawlDays < out.RecrawlDays {
		out.RecrawlDays = b.RecrawlDays
	}
	return out
}

// stricter returns the stricter of two floor sets: larger gap, smaller
// per-cycle page budget, longer recrawl window. Zero values in b mean
// "unset" and leave a's dimension alone.
func stricter(a, b Floors) Floors {
	out := a
	if b.MinHostGap > 0 && b.MinHostGap > out.MinHostGap {
		out.MinHostGap = b.MinHostGap
	}
	if b.MaxPagesPerHostCycle > 0 && (out.MaxPagesPerHostCycle <= 0 || b.MaxPagesPerHostCycle < out.MaxPagesPerHostCycle) {
		out.MaxPagesPerHostCycle = b.MaxPagesPerHostCycle
	}
	if b.RecrawlDays > 0 && b.RecrawlDays > out.RecrawlDays {
		out.RecrawlDays = b.RecrawlDays
	}
	return out
}

// ApplyPushedFloors folds a central config push into the effective floors.
//
//   - A pushed floor is adopted only if it is STRICTER than the local
//     floor (the server can tighten fleet politeness, never loosen a node
//     past its operator's own bounds).
//   - One-directional: the adopted central values only ever move gentler.
//     A later push that would re-tighten past a previous relaxation is
//     ignored for the adopted component.
//   - heartbeat_interval_s is authoritative from the latest push (not
//     floor-strictness): the node must beat at the cadence the server uses
//     for its 3-missed-beats dead detection, or it gets declared dead.
//
// A zero/negative pushed value means "unset" and is ignored.
func ApplyPushedFloors(local Floors, pushed PushedConfig, st *floorState) Floors {
	p := Floors{
		MinHostGap:           time.Duration(pushed.MinHostGapMs) * time.Millisecond,
		MaxPagesPerHostCycle: pushed.MaxPagesPerHostCycle,
		RecrawlDays:          pushed.RecrawlDays,
	}

	if st.hasAdopted {
		// One-directional: adopted central floors can only get gentler.
		p = gentler(st.adopted, p)
	} else if p != (Floors{}) {
		st.hasAdopted = true
	}
	st.adopted = p

	eff := stricter(local, st.adopted)

	// Heartbeat interval is authoritative: survival depends on matching the
	// server's dead-detection cadence.
	if pushed.HeartbeatIntervalS > 0 {
		eff.HeartbeatInterval = time.Duration(pushed.HeartbeatIntervalS) * time.Second
	} else if eff.HeartbeatInterval <= 0 {
		eff.HeartbeatInterval = local.HeartbeatInterval
	}
	return eff
}

// LeaseWindow returns how long a claim lease lasts: 2× the node's p99
// ingest-batch latency, floored at 5min and capped at 30min. A non-positive
// p99 (unknown — the first claim, pre-p99) yields the 10min default.
func LeaseWindow(p99Batch time.Duration) time.Duration {
	const (
		floor = 5 * time.Minute
		cap   = 30 * time.Minute
		deflt = 10 * time.Minute
	)
	if p99Batch <= 0 {
		return deflt
	}
	w := 2 * p99Batch
	if w < floor {
		return floor
	}
	if w > cap {
		return cap
	}
	return w
}
