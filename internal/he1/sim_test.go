package he1

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

// simDriver is one virtual node: a real Coordinator + Client speaking to the
// FakeCentral over HTTP, with an independent audit log of every grant and
// ingest it performed. alive=false models a dead node (heartbeats stop).
type simDriver struct {
	id       string
	coord    *Coordinator
	fc       *FakeCentral
	alive    bool
	granted  map[string]int // domain id -> times this node was granted it
	ingested map[string]int // domain id -> times this node ingested it
}

func newSimDriver(t *testing.T, fc *FakeCentral, srv string, id string, shards int, p99 time.Duration) *simDriver {
	t.Helper()
	d := &simDriver{
		id: id, fc: fc, alive: true,
		granted: map[string]int{}, ingested: map[string]int{},
	}
	d.coord = NewCoordinator(id, NewClient(srv, "tok-"+id), Floors{
		MinHostGap:           2 * time.Second,
		MaxPagesPerHostCycle: 25,
		RecrawlDays:          7,
		HeartbeatInterval:    30 * time.Second,
	}, shards)
	fc.SetNodeP99(id, p99)
	return d
}

// simWorld bundles a fake central, virtual clock, and httptest server.
type simWorld struct {
	fc     *FakeCentral
	vc     *vclock
	shards int
}

func newSimWorld(t *testing.T, domains, shards int) (*simWorld, string) {
	t.Helper()
	vc := newVClock()
	fc := NewFakeCentral(domains, shards, vc.now)
	srv := httptest.NewServer(fc)
	t.Cleanup(srv.Close)
	return &simWorld{fc: fc, vc: vc, shards: shards}, srv.URL
}

func (w *simWorld) addDriver(t *testing.T, srv string, id string) *simDriver {
	t.Helper()
	return newSimDriver(t, w.fc, srv, id, w.shards, 0)
}

// step runs one 30s round for the driver: heartbeat, claim, ingest a
// fraction of the batch (ingestFrac 1.0 = everything, immediately).
func (d *simDriver) step(ctx context.Context, claimN int, ingestFrac float64) {
	if !d.alive {
		return
	}
	if err := d.coord.Heartbeat(ctx); err != nil {
		panic(err)
	}
	lid, doms, err := d.coord.Claim(ctx, claimN)
	if err != nil {
		panic(err)
	}
	for _, dm := range doms {
		d.granted[dm.ID]++
	}
	k := int(float64(len(doms)) * ingestFrac)
	if k > 0 && lid != "" {
		ids := idsOf(doms[:k])
		d.fc.Ingest(d.id, lid, ids)
		d.coord.Ingested(lid, ids)
		for _, id := range ids {
			d.ingested[id]++
		}
	}
}

// outstanding returns domain ids granted to this driver but never ingested.
func (d *simDriver) outstanding() map[string]bool {
	out := map[string]bool{}
	for id := range d.granted {
		if d.ingested[id] == 0 {
			out[id] = true
		}
	}
	return out
}

// HE-1 core invariant 1: with N live nodes, no domain is ever double-crawled
// and every domain is crawled exactly once.
func TestSimNoDoubleCrawl(t *testing.T) {
	const domains, shards, nodes = 2000, 16, 4
	w, srv := newSimWorld(t, domains, shards)
	ctx := context.Background()

	var drivers []*simDriver
	for i := 0; i < nodes; i++ {
		drivers = append(drivers, w.addDriver(t, srv, "nd-"+string(rune('a'+i))))
	}

	for round := 0; round < 40; round++ {
		for _, d := range drivers {
			d.step(ctx, 100, 1.0)
		}
		w.vc.advance(30 * time.Second)
	}

	// Every domain ingested exactly once, by exactly one node.
	total := 0
	for _, d := range drivers {
		for id, n := range d.ingested {
			if n != 1 {
				t.Fatalf("domain %s ingested %d times by %s", id, n, d.id)
			}
			total++
		}
		for id, n := range d.granted {
			if n != 1 {
				t.Fatalf("domain %s granted %d times to %s (overlapping leases?)", id, n, d.id)
			}
		}
	}
	if total != domains {
		t.Fatalf("ingested %d unique domains, want %d", total, domains)
	}
}

// HE-1 core invariant 2: when a node dies (90s without a beat), its shards
// are reassigned, its unexpired leases are NOT double-granted, and once they
// expire its domains are reclaimed — no-double-crawl + no-domain-lost in one
// mechanism.
func TestSimNodeDeathNoDomainLost(t *testing.T) {
	const domains, shards = 600, 8
	w, srv := newSimWorld(t, domains, shards)
	ctx := context.Background()

	a := w.addDriver(t, srv, "nd-a")
	b := w.addDriver(t, srv, "nd-b")
	c := w.addDriver(t, srv, "nd-c")

	// Two rounds with b ingesting only half its batches (slow node with
	// in-flight work when it dies).
	for round := 0; round < 2; round++ {
		a.step(ctx, 50, 1.0)
		b.step(ctx, 50, 0.5)
		c.step(ctx, 50, 1.0)
		w.vc.advance(30 * time.Second)
	}
	bOutstanding := b.outstanding()
	if len(bOutstanding) == 0 {
		t.Fatal("test setup: b should hold un-ingested leased domains")
	}
	epochBefore := a.coord.Epoch()

	// Kill b: heartbeats stop. Advance past the 90s dead threshold.
	b.alive = false
	for i := 0; i < 4; i++ {
		a.step(ctx, 50, 1.0)
		c.step(ctx, 50, 1.0)
		w.vc.advance(30 * time.Second)
	}

	// b is dead: shards redealt, epoch bumped on the survivors.
	for _, id := range w.fc.LiveNodes() {
		if id == "nd-b" {
			t.Fatal("nd-b still listed live 120s after its last beat")
		}
	}
	if a.coord.Epoch() == epochBefore || c.coord.Epoch() == epochBefore {
		t.Fatal("membership epoch did not bump after node death")
	}
	held := append(a.coord.shards.List(), c.coord.shards.List()...)
	if len(held) != shards {
		t.Fatalf("survivors hold %d shards, want %d (reassignment)", len(held), shards)
	}

	// While b's leases are unexpired, survivors must NOT be granted b's
	// outstanding domains (no double-crawl across death).
	a.step(ctx, 50, 1.0)
	c.step(ctx, 50, 1.0)
	for id := range bOutstanding {
		if a.granted[id] > 0 || c.granted[id] > 0 {
			t.Fatalf("domain %s re-granted while nd-b's lease unexpired", id)
		}
	}

	// Past the lease window (10min default, b never reported a p99):
	// b's domains become reclaimable and get crawled. Nothing is lost.
	w.vc.advance(11 * time.Minute)
	for round := 0; round < 30; round++ {
		a.step(ctx, 50, 1.0)
		c.step(ctx, 50, 1.0)
		w.vc.advance(30 * time.Second)
	}
	seen := map[string]int{}
	for _, d := range []*simDriver{a, b, c} {
		for id, n := range d.ingested {
			seen[id] += n
		}
	}
	if len(seen) != domains {
		t.Fatalf("crawled %d unique domains, want %d — domains lost", len(seen), domains)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("domain %s crawled %d times", id, n)
		}
	}
	for id := range bOutstanding {
		if seen[id] != 1 {
			t.Fatalf("dead node's domain %s never reclaimed", id)
		}
	}
}

// Membership change: a joining node bumps the epoch; holders re-filter their
// draw so a shard is drawn by exactly one node.
func TestSimMembershipEpochRefilter(t *testing.T) {
	w, srv := newSimWorld(t, 200, 8)
	ctx := context.Background()

	a := w.addDriver(t, srv, "nd-a")
	b := w.addDriver(t, srv, "nd-b")
	a.step(ctx, 1, 1.0)
	b.step(ctx, 1, 1.0)
	a.step(ctx, 1, 1.0) // observe the epoch after b's join
	if a.coord.Epoch() != 2 {
		t.Fatalf("epoch = %d, want 2 after two joins", a.coord.Epoch())
	}
	// With 2 nodes on 8 shards, nd-a holds the evens incl. shard 2.
	if !a.coord.OwnsShard(2) {
		t.Fatal("setup: nd-a should hold shard 2 with 2 nodes")
	}

	c := w.addDriver(t, srv, "nd-c")
	c.step(ctx, 1, 1.0) // join bumps the epoch
	if w.fc.Epoch() != 3 {
		t.Fatalf("central epoch = %d, want 3 after join", w.fc.Epoch())
	}
	a.step(ctx, 1, 1.0)
	if a.coord.Epoch() != 3 {
		t.Fatal("nd-a did not observe the epoch bump")
	}
	// Shard 2 moved to nd-c (round-robin re-deal); nd-a must stop drawing it.
	if a.coord.OwnsShard(2) {
		t.Error("nd-a still draws shard 2 after losing it")
	}
	if !c.coord.OwnsShard(2) {
		t.Error("nd-c does not hold shard 2 after re-deal")
	}
	var shard2dom string
	for i := 0; i < 200; i++ {
		if d := domainFor(i); ShardOf(d, 8) == 2 {
			shard2dom = d
			break
		}
	}
	if shard2dom == "" {
		t.Fatal("no synthetic domain in shard 2")
	}
	if a.coord.OwnsDomain(shard2dom) {
		t.Errorf("nd-a still draws %s (shard 2) after epoch change", shard2dom)
	}
	if !c.coord.OwnsDomain(shard2dom) {
		t.Errorf("nd-c does not draw %s (shard 2) after epoch change", shard2dom)
	}
}

// Lease renewal via heartbeat: a renewed lease stays out of the claimable
// pool; without renewal it expires and is reclaimed. Also covers the 10min
// default window for a node with no p99 yet.
func TestSimLeaseRenewalAndExpiry(t *testing.T) {
	w, srv := newSimWorld(t, 100, 4)
	ctx := context.Background()

	n1 := w.addDriver(t, srv, "nd-1")
	w.fc.SetNodeP99("nd-1", 4*time.Minute) // window = 8min
	n1.step(ctx, 10, 0)                    // claim, ingest nothing
	lid := ""
	for id := range w.fc.ActiveLeases() {
		lid = id
	}
	if lid == "" {
		t.Fatal("no lease created")
	}
	exp, _ := w.fc.LeaseExpiry(lid)
	if exp.Sub(w.vc.now()) != 8*time.Minute {
		t.Fatalf("lease window = %v, want 8min (2× p99)", exp.Sub(w.vc.now()))
	}

	// Renew at t+7min: expiry extends to t+15min.
	w.vc.advance(7 * time.Minute)
	if err := n1.coord.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	exp, _ = w.fc.LeaseExpiry(lid)
	if exp.Sub(w.vc.now()) != 8*time.Minute {
		t.Fatalf("renewed lease window = %v, want 8min from renewal", exp.Sub(w.vc.now()))
	}

	// A second node cannot claim the renewed domains.
	n2 := w.addDriver(t, srv, "nd-2")
	n2.step(ctx, 100, 1.0)
	for id := range n1.granted {
		if n2.granted[id] > 0 {
			t.Fatalf("domain %s granted to nd-2 while nd-1's lease live", id)
		}
	}

	// Heartbeats stop → lease expires → domains reclaimable.
	n1.alive = false
	w.vc.advance(9 * time.Minute)
	n2.step(ctx, 100, 1.0)
	reclaimed := 0
	for id := range n1.granted {
		if n2.granted[id] > 0 {
			reclaimed++
		}
	}
	if reclaimed != len(n1.granted) {
		t.Fatalf("reclaimed %d/%d domains after lease expiry", reclaimed, len(n1.granted))
	}
}

// First claim pre-p99 uses the 10min default window.
func TestSimDefaultLeaseWindow(t *testing.T) {
	w, srv := newSimWorld(t, 50, 4)
	ctx := context.Background()
	n := w.addDriver(t, srv, "nd-new")
	n.step(ctx, 5, 0)
	for lid := range w.fc.ActiveLeases() {
		exp, _ := w.fc.LeaseExpiry(lid)
		if exp.Sub(w.vc.now()) != 10*time.Minute {
			t.Fatalf("default lease window = %v, want 10min", exp.Sub(w.vc.now()))
		}
	}
}
