// FakeCentral: a fake PROTOCOL v2 central for the HE-1 simulation harness.
//
// It implements the server semantics the wire promises, so the client
// (wire.go + coordinator.go + shard.go) can be proven end-to-end before any
// prod central exists:
//
//   - Shard ring: shards are dealt round-robin over live nodes; join, leave,
//     and death re-deal and bump the membership epoch.
//   - Lease table: the central is the SOLE writer of lease state. Claimable =
//     not crawled within the recrawl window AND not under an unexpired lease.
//   - Dead detection: 3 missed 30s beats (90s) = dead. Shards are reassigned
//     immediately; leases are NOT force-released — they expire naturally and
//     become reclaimable within one window (no-double-crawl + no-domain-lost
//     in one mechanism).
//   - Heartbeat renews in-flight leases: expiresAt = now + LeaseWindow(p99).
//   - Ingest (the existing /api/ingest path) implicitly releases the lease
//     for ingested domains.
//
// Time is virtual: tests drive now via Advance, so 90s dead detection and
// 30min lease caps run in milliseconds. This is a test fake, not a prod
// server — the real central (node_leases table, real ring, admin UI) is
// Claude's lane.
package he1

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// DeadAfter is the dead-detection threshold: 3 missed 30s beats.
const DeadAfter = 90 * time.Second

type simNode struct {
	id       string
	lastBeat time.Time
	p99Batch time.Duration
	shards   []uint16
}

type simLease struct {
	id        string
	nodeID    string
	domainIDs []string
	expiresAt time.Time
}

type simDomain struct {
	id            string
	domain        string
	shard         uint16
	lastCrawled   time.Time // zero = never crawled
	lastCrawledBy string    // node that last ingested it (audit aid)
}

// FakeCentral is the fake v2 central. ServeHTTP exposes the three endpoints;
// the Ingest/Advance/SetConfig/Kill helpers model the rest of the world.
type FakeCentral struct {
	mu         sync.Mutex
	now        func() time.Time
	shardCount int
	epoch      int
	nodes      map[string]*simNode
	leases     map[string]*simLease
	domains    map[string]*simDomain
	byShard    map[uint16][]string      // shard -> domain ids, id-sorted
	domainGone map[string]string        // domain id -> active lease id
	pendingP99 map[string]time.Duration // p99 set before a node joins
	config     PushedConfig
	nextLease  int
	recrawl    time.Duration
}

// NewFakeCentral builds a fake central over a synthetic pool of domainCount
// domains spread across shardCount shards.
func NewFakeCentral(domainCount, shardCount int, now func() time.Time) *FakeCentral {
	if shardCount <= 0 {
		shardCount = DefaultShardCount
	}
	fc := &FakeCentral{
		now:        now,
		shardCount: shardCount,
		nodes:      make(map[string]*simNode),
		leases:     make(map[string]*simLease),
		domains:    make(map[string]*simDomain),
		byShard:    make(map[uint16][]string),
		domainGone: make(map[string]string),
		pendingP99: make(map[string]time.Duration),
		recrawl:    7 * 24 * time.Hour,
	}
	for i := 0; i < domainCount; i++ {
		d := &simDomain{id: leaseIDFor(i), domain: domainFor(i)}
		d.shard = ShardOf(d.domain, shardCount)
		fc.domains[d.id] = d
		fc.byShard[d.shard] = append(fc.byShard[d.shard], d.id)
	}
	for sh, ids := range fc.byShard {
		sort.Strings(ids)
		fc.byShard[sh] = ids
	}
	return fc
}

func leaseIDFor(i int) string { return "dom-" + pad6(i) }

// Distinct registrable domains (siteNNNNNN.com), so the pool spreads across
// the shard space the way real multi-site crawls do.
func domainFor(i int) string { return "site" + pad6(i) + ".com" }
func pad6(i int) string {
	s := "000000"
	b := []byte(s)
	for j := 5; i > 0 && j >= 0; j-- {
		b[j] = byte('0' + i%10)
		i /= 10
	}
	return string(b)
}

// SetConfig sets the config pushed on heartbeats.
func (fc *FakeCentral) SetConfig(c PushedConfig) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.config = c
}

// SetRecrawlDays sets the recrawl window used for claimability.
func (fc *FakeCentral) SetRecrawlDays(days int) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.recrawl = time.Duration(days) * 24 * time.Hour
}

// SetNodeP99 sets a node's simulated p99 ingest-batch latency (drives its
// lease windows). Safe to call before the node joins; the value applies on
// join.
func (fc *FakeCentral) SetNodeP99(nodeID string, p99 time.Duration) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.pendingP99[nodeID] = p99
	if n, ok := fc.nodes[nodeID]; ok {
		n.p99Batch = p99
	}
}

// LiveNodes returns the ids of nodes not currently dead.
func (fc *FakeCentral) LiveNodes() []string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.sweepLocked()
	var out []string
	for id := range fc.nodes {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Epoch returns the current membership epoch.
func (fc *FakeCentral) Epoch() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.epoch
}

// LeaseExpiry returns the current expiry of a lease ("" if gone).
func (fc *FakeCentral) LeaseExpiry(leaseID string) (time.Time, bool) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	l, ok := fc.leases[leaseID]
	if !ok {
		return time.Time{}, false
	}
	return l.expiresAt, true
}

// Ingest models the existing /api/ingest path: submitting domains
// IMPLICITLY releases the lease for the ingested domains (wire §2) and marks
// them crawled.
func (fc *FakeCentral) Ingest(nodeID, leaseID string, domainIDs []string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	now := fc.now()
	for _, did := range domainIDs {
		if d, ok := fc.domains[did]; ok {
			d.lastCrawled = now
			d.lastCrawledBy = nodeID
		}
		delete(fc.domainGone, did)
	}
	if l, ok := fc.leases[leaseID]; ok && l.nodeID == nodeID {
		keep := l.domainIDs[:0]
		gone := make(map[string]struct{}, len(domainIDs))
		for _, did := range domainIDs {
			gone[did] = struct{}{}
		}
		for _, did := range l.domainIDs {
			if _, ok := gone[did]; !ok {
				keep = append(keep, did)
			}
		}
		l.domainIDs = keep
		if len(keep) == 0 {
			delete(fc.leases, leaseID)
		}
	}
}

// CrawledBy returns domain id -> node id for every crawled domain.
func (fc *FakeCentral) CrawledBy() map[string]string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	out := make(map[string]string, len(fc.domains))
	for id, d := range fc.domains {
		if !d.lastCrawled.IsZero() {
			out[id] = d.lastCrawledBy
		}
	}
	return out
}

// ActiveLeases returns lease id -> node id for live leases.
func (fc *FakeCentral) ActiveLeases() map[string]string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	now := fc.now()
	out := make(map[string]string)
	for id, l := range fc.leases {
		if now.Before(l.expiresAt) {
			out[id] = l.nodeID
		}
	}
	return out
}

// sweepLocked drops nodes past the dead threshold and re-deals the ring.
// Leases of dead nodes are left to expire naturally (reclaimable within one
// window) — the no-domain-lost mechanism.
func (fc *FakeCentral) sweepLocked() {
	now := fc.now()
	var dead []string
	for id, n := range fc.nodes {
		if now.Sub(n.lastBeat) > DeadAfter {
			dead = append(dead, id)
		}
	}
	if len(dead) == 0 {
		return
	}
	sort.Strings(dead)
	for _, id := range dead {
		delete(fc.nodes, id)
	}
	fc.redealLocked()
}

func (fc *FakeCentral) redealLocked() {
	ids := make([]string, 0, len(fc.nodes))
	for id := range fc.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, n := range fc.nodes {
		n.shards = n.shards[:0]
	}
	for sh := 0; sh < fc.shardCount; sh++ {
		if len(ids) == 0 {
			break
		}
		n := fc.nodes[ids[sh%len(ids)]]
		n.shards = append(n.shards, uint16(sh))
	}
	fc.epoch++
}

// ServeHTTP routes the three v2 endpoints.
func (fc *FakeCentral) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/nodes/heartbeat":
		fc.heartbeat(w, r)
	case "/api/frontier/claim":
		fc.claim(w, r)
	case "/api/frontier/release":
		fc.release(w, r)
	default:
		http.NotFound(w, r)
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return false
	}
	return true
}

func encode(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (fc *FakeCentral) heartbeat(w http.ResponseWriter, r *http.Request) {
	var req HeartbeatReq
	if !decode(w, r, &req) {
		return
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.sweepLocked()
	now := fc.now()

	n, ok := fc.nodes[req.NodeID]
	if !ok {
		// Join (or rejoin after death): deal the ring, bump the epoch.
		n = &simNode{id: req.NodeID, p99Batch: fc.pendingP99[req.NodeID]}
		fc.nodes[req.NodeID] = n
		fc.redealLocked()
	}
	n.lastBeat = now

	// Renew in-flight leases the node is still working.
	renewed := make(map[string]string, len(req.LeaseRenewals))
	for _, lid := range req.LeaseRenewals {
		if l, ok := fc.leases[lid]; ok && l.nodeID == req.NodeID && now.Before(l.expiresAt) {
			l.expiresAt = now.Add(LeaseWindow(n.p99Batch))
			renewed[lid] = l.expiresAt.UTC().Format(time.RFC3339)
		}
	}

	encode(w, HeartbeatResp{
		ShardsAssigned:    append([]uint16(nil), n.shards...),
		LeaseRenewedUntil: renewed,
		Config:            fc.config,
		MembershipEpoch:   fc.epoch,
	})
}

// claimableLocked reports whether a domain may be claimed right now.
func (fc *FakeCentral) claimableLocked(d *simDomain, now time.Time) bool {
	if _, leased := fc.domainGone[d.id]; leased {
		if l, ok := fc.leases[fc.domainGone[d.id]]; ok && now.Before(l.expiresAt) {
			return false
		}
		// Lease record gone/expired: fall through (stale index entry).
		delete(fc.domainGone, d.id)
	}
	if !d.lastCrawled.IsZero() && now.Sub(d.lastCrawled) < fc.recrawl {
		return false
	}
	return true
}

func (fc *FakeCentral) claim(w http.ResponseWriter, r *http.Request) {
	var req ClaimReq
	if !decode(w, r, &req) {
		return
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.sweepLocked()
	now := fc.now()

	n, ok := fc.nodes[req.NodeID]
	if !ok {
		http.Error(w, "unknown node: heartbeat first", http.StatusUnauthorized)
		return
	}
	count := req.Count
	if count > MaxClaimCount {
		count = MaxClaimCount
	}

	// Only grant from shards the node actually holds (intersect).
	held := make(map[uint16]struct{}, len(n.shards))
	for _, sh := range n.shards {
		held[sh] = struct{}{}
	}
	var shards []uint16
	for _, sh := range req.Shards {
		if _, ok := held[sh]; ok {
			shards = append(shards, sh)
		}
	}
	sort.Slice(shards, func(i, j int) bool { return shards[i] < shards[j] })

	var picked []ClaimedDomain
outer:
	for _, sh := range shards {
		for _, did := range fc.byShard[sh] {
			if len(picked) >= count {
				break outer
			}
			d := fc.domains[did]
			if !fc.claimableLocked(d, now) {
				continue
			}
			picked = append(picked, ClaimedDomain{ID: d.id, Domain: d.domain, Shard: d.shard})
		}
	}

	fc.nextLease++
	window := LeaseWindow(n.p99Batch)
	exp := now.Add(window)
	ids := make([]string, 0, len(picked))
	lid := ""
	if len(picked) > 0 {
		lid = "lease-" + pad6(fc.nextLease)
		for _, p := range picked {
			ids = append(ids, p.ID)
			fc.domainGone[p.ID] = lid
		}
		fc.leases[lid] = &simLease{id: lid, nodeID: req.NodeID, domainIDs: ids, expiresAt: exp}
	}

	encode(w, ClaimResp{
		LeaseID:    lid,
		LeaseUntil: exp.UTC().Format(time.RFC3339),
		Domains:    picked,
	})
}

func (fc *FakeCentral) release(w http.ResponseWriter, r *http.Request) {
	var req ReleaseReq
	if !decode(w, r, &req) {
		return
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for _, did := range req.DomainIDs {
		delete(fc.domainGone, did)
	}
	for _, l := range fc.leases {
		if l.nodeID != req.NodeID {
			continue
		}
		skip := make(map[string]struct{}, len(req.DomainIDs))
		for _, did := range req.DomainIDs {
			skip[did] = struct{}{}
		}
		keep := l.domainIDs[:0]
		for _, did := range l.domainIDs {
			if _, ok := skip[did]; !ok {
				keep = append(keep, did)
			}
		}
		l.domainIDs = keep
	}
	encode(w, map[string]string{"status": "ok"})
}
