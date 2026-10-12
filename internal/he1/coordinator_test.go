package he1

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// vclock is a manually advanced clock for the sim.
type vclock struct {
	mu sync.Mutex
	t  time.Time
}

func newVClock() *vclock { return &vclock{t: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)} }
func (v *vclock) now() time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.t
}
func (v *vclock) advance(d time.Duration) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.t = v.t.Add(d)
}

// testPair spins up a fake central + a coordinator speaking to it.
func testPair(t *testing.T, domains, shards int) (*FakeCentral, *vclock, *Coordinator) {
	t.Helper()
	vc := newVClock()
	fc := NewFakeCentral(domains, shards, vc.now)
	srv := httptest.NewServer(fc)
	t.Cleanup(srv.Close)
	coord := NewCoordinator("nd_test", NewClient(srv.URL, "tok"), Floors{
		MinHostGap:           2 * time.Second,
		MaxPagesPerHostCycle: 25,
		RecrawlDays:          7,
		HeartbeatInterval:    30 * time.Second,
	}, shards)
	return fc, vc, coord
}

func TestCoordinatorHeartbeatAppliesAssignment(t *testing.T) {
	_, _, coord := testPair(t, 100, 8)
	ctx := context.Background()

	if err := coord.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if coord.Epoch() != 1 {
		t.Errorf("first heartbeat epoch = %d, want 1", coord.Epoch())
	}
	held := coord.shards.List()
	if len(held) != 8 {
		t.Errorf("single node should hold all 8 shards, holds %d", len(held))
	}
	if !coord.OwnsDomain("site000001.example.com") {
		t.Error("node should draw domains in its shards")
	}
}

// A pushed config arriving via heartbeat folds into effective floors.
func TestCoordinatorAppliesPushedConfig(t *testing.T) {
	fc, _, coord := testPair(t, 100, 8)
	fc.SetConfig(PushedConfig{MinHostGapMs: 5000, MaxPagesPerHostCycle: 10, RecrawlDays: 30, HeartbeatIntervalS: 45})
	if err := coord.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	eff := coord.Floors()
	if eff.MinHostGap != 5*time.Second || eff.MaxPagesPerHostCycle != 10 || eff.RecrawlDays != 30 {
		t.Errorf("pushed floors not applied: %+v", eff)
	}
	if eff.HeartbeatInterval != 45*time.Second {
		t.Errorf("heartbeat interval not authoritative: %v", eff.HeartbeatInterval)
	}
}

// Claim tracks the lease; Ingested drains it; InFlight counts outstanding.
func TestCoordinatorClaimAndIngest(t *testing.T) {
	fc, _, coord := testPair(t, 100, 8)
	ctx := context.Background()
	if err := coord.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	_, doms, err := coord.Claim(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(doms) != 10 {
		t.Fatalf("claimed %d domains, want 10", len(doms))
	}
	if coord.InFlight() != 10 {
		t.Errorf("in-flight = %d, want 10", coord.InFlight())
	}
	// Ingest half (models /api/ingest implicitly releasing those).
	fc.Ingest("nd_test", leaseOf(t, fc, "nd_test"), idsOf(doms[:5]))
	coord.Ingested(leaseOf(t, fc, "nd_test"), idsOf(doms[:5]))
	if coord.InFlight() != 5 {
		t.Errorf("in-flight after partial ingest = %d, want 5", coord.InFlight())
	}
	// The node keeps renewing the remainder on heartbeat.
	if err := coord.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if coord.InFlight() != 5 {
		t.Errorf("heartbeat must not drop un-ingested domains, in-flight = %d", coord.InFlight())
	}
}

func leaseOf(t *testing.T, fc *FakeCentral, node string) string {
	t.Helper()
	for id, owner := range fc.ActiveLeases() {
		if owner == node {
			return id
		}
	}
	t.Fatalf("no active lease for %s", node)
	return ""
}

func idsOf(doms []ClaimedDomain) []string {
	out := make([]string, 0, len(doms))
	for _, d := range doms {
		out = append(out, d.ID)
	}
	return out
}
