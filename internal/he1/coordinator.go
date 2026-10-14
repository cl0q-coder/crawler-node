// Coordinator: client-side HE-1 state machine (HE-1, issue #135).
//
// The Coordinator owns everything the node must track across heartbeats:
// the currently held shards, the membership epoch (re-filter the draw when
// it moves), in-flight leases (renewed via heartbeat), and the effective
// politeness floors (one-directional per ApplyPushedFloors).
//
// Draw rule: the crawler only draws domains whose ShardOf(domain) is in the
// current ShardSet AND whose lease came from a claim against the current
// epoch. When the epoch bumps, shards the node no longer holds are dropped
// from the draw immediately — the central reassigns them, so another node
// will claim them; in-flight leases on lost shards are still renewed (and
// their domains still ingested) until they drain, because ingest implicitly
// releases and the lease is the central's single source of truth.
package he1

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// activeLease is one claim the node is working through.
type activeLease struct {
	id           string
	domains      []ClaimedDomain // not yet ingested
	until        time.Time
	renewedUntil time.Time
}

// Coordinator drives heartbeat/claim against a v2 central.
type Coordinator struct {
	nodeID     string
	client     *Client
	shardCount int

	mu       sync.Mutex
	local    Floors
	floorSt  floorState
	eff      Floors
	shards   *ShardSet
	epoch    int
	hasEpoch bool
	leases   map[string]*activeLease
	metrics  NodeMetrics
}

// NewCoordinator builds the coordinator. local is the operator's own
// politeness floors (never overridden past stricter); shardCount sizes the
// ShardOf space (DefaultShardCount when <= 0).
func NewCoordinator(nodeID string, client *Client, local Floors, shardCount int) *Coordinator {
	if shardCount <= 0 {
		shardCount = DefaultShardCount
	}
	c := &Coordinator{
		nodeID:     nodeID,
		client:     client,
		shardCount: shardCount,
		local:      local,
		eff:        local,
		shards:     NewShardSet(nil),
		leases:     make(map[string]*activeLease),
	}
	return c
}

// SetMetrics sets the telemetry reported on the next heartbeat.
func (c *Coordinator) SetMetrics(m NodeMetrics) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.metrics = m
}

// Floors returns the currently effective politeness floors.
func (c *Coordinator) Floors() Floors {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.eff
}

// Epoch returns the last membership epoch seen.
func (c *Coordinator) Epoch() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epoch
}

// OwnsShard reports whether domain's shard is currently held and drawable.
func (c *Coordinator) OwnsShard(shard uint16) bool {
	return c.shards.Contains(shard)
}

// OwnsDomain reports whether the node should draw this domain right now.
func (c *Coordinator) OwnsDomain(domain string) bool {
	return c.shards.Contains(ShardOf(domain, c.shardCount))
}

// InFlight returns the number of claimed-but-not-ingested domains.
func (c *Coordinator) InFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, l := range c.leases {
		n += len(l.domains)
	}
	return n
}

// renewals lists the lease ids the next heartbeat should renew.
func (c *Coordinator) renewals() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.leases))
	for id := range c.leases {
		out = append(out, id)
	}
	return out
}

// Heartbeat sends the 30s heartbeat and applies the central's response:
// new shard assignment (re-filtering the draw on epoch change), lease
// renewal timestamps, and pushed config floors.
func (c *Coordinator) Heartbeat(ctx context.Context) error {
	req := HeartbeatReq{
		NodeID:        c.nodeID,
		ShardsHeld:    c.shards.List(),
		InFlightCount: c.InFlight(),
		LeaseRenewals: c.renewals(),
	}
	c.mu.Lock()
	req.Metrics = c.metrics
	c.mu.Unlock()

	resp, err := c.client.Heartbeat(ctx, req)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Membership change: re-filter the draw to the new assignment.
	if !c.hasEpoch || resp.MembershipEpoch != c.epoch {
		c.epoch = resp.MembershipEpoch
		c.hasEpoch = true
		c.shards.Replace(resp.ShardsAssigned)
	}

	// Lease renewal timestamps from the central (sole writer of lease state).
	for id, ts := range resp.LeaseRenewedUntil {
		if l, ok := c.leases[id]; ok {
			if t, err := LeaseUntil(ts); err == nil {
				l.renewedUntil = t
			}
		}
	}

	c.eff = ApplyPushedFloors(c.local, resp.Config, &c.floorSt)
	return nil
}

// Claim requests up to count domains from currently held shards and tracks
// the returned lease for renewal until it drains. It returns the lease id
// ("" when nothing was granted) alongside the domains.
func (c *Coordinator) Claim(ctx context.Context, count int) (string, []ClaimedDomain, error) {
	if count <= 0 {
		return "", nil, nil
	}
	resp, err := c.client.Claim(ctx, ClaimReq{
		NodeID: c.nodeID,
		Count:  count,
		Shards: c.shards.List(),
	})
	if err != nil {
		return "", nil, err
	}
	until, err := LeaseUntil(resp.LeaseUntil)
	if err != nil {
		return "", nil, fmt.Errorf("he1: bad lease_until %q: %w", resp.LeaseUntil, err)
	}
	if resp.LeaseID != "" {
		c.mu.Lock()
		c.leases[resp.LeaseID] = &activeLease{
			id:           resp.LeaseID,
			domains:      resp.Domains,
			until:        until,
			renewedUntil: until,
		}
		c.mu.Unlock()
	}
	return resp.LeaseID, resp.Domains, nil
}

// Ingested records that domains from a lease reached the central via
// /api/ingest, which IMPLICITLY releases the lease for them (wire §2). The
// node stops renewing for ingested domains; the lease entry is dropped when
// it drains.
func (c *Coordinator) Ingested(leaseID string, domainIDs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.leases[leaseID]
	if !ok {
		return
	}
	done := make(map[string]struct{}, len(domainIDs))
	for _, id := range domainIDs {
		done[id] = struct{}{}
	}
	keep := l.domains[:0]
	for _, d := range l.domains {
		if _, ok := done[d.ID]; !ok {
			keep = append(keep, d)
		}
	}
	l.domains = keep
	if len(keep) == 0 {
		delete(c.leases, leaseID)
	}
}

// Release tells the central the node is skipping these domains (optional
// wire §3) and drops them from local tracking.
func (c *Coordinator) Release(ctx context.Context, leaseID string, domainIDs []string) error {
	if err := c.client.Release(ctx, ReleaseReq{NodeID: c.nodeID, DomainIDs: domainIDs}); err != nil {
		return err
	}
	c.Ingested(leaseID, domainIDs) // same local bookkeeping: stop renewing
	return nil
}
