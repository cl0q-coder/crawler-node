package he1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Additive wire: the server may return fields the client doesn't know; the
// client must ignore them, not fail.
func TestWireIgnoresUnknownFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/nodes/heartbeat" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"shards_assigned": [3, 7],
				"lease_renewed_until": {"lease-1": "2026-10-12T12:00:00Z"},
				"config": {"min_host_gap_ms": 2000},
				"membership_epoch": 4,
				"future_field_we_dont_know": {"nested": true},
				"another_one": [1,2,3]
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	resp, err := c.Heartbeat(context.Background(), HeartbeatReq{NodeID: "nd_x"})
	if err != nil {
		t.Fatalf("heartbeat with unknown fields: %v", err)
	}
	if resp.MembershipEpoch != 4 || len(resp.ShardsAssigned) != 2 {
		t.Errorf("bad decode: %+v", resp)
	}
}

// The client caps claims at 2000 even if asked for more.
func TestClaimCapsAt2000(t *testing.T) {
	var gotCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ClaimReq
		json.NewDecoder(r.Body).Decode(&req)
		gotCount = req.Count
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"lease_id":"","lease_until":"2026-10-12T12:00:00Z","domains":[]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if _, err := c.Claim(context.Background(), ClaimReq{NodeID: "n", Count: 5000}); err != nil {
		t.Fatal(err)
	}
	if gotCount != MaxClaimCount {
		t.Errorf("claim count sent = %d, want cap %d", gotCount, MaxClaimCount)
	}
}

// Bearer auth is sent; redirects are refused (token never leaves the host).
func TestClientAuthAndNoRedirect(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		http.Redirect(w, r, "http://evil.example/", http.StatusFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "secret-token")
	err := c.Release(context.Background(), ReleaseReq{NodeID: "n", DomainIDs: []string{"a"}})
	if err == nil {
		t.Fatal("expected redirect refusal, got nil")
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization header = %q", gotAuth)
	}
}

// protocol_version is always advertised as 2 on heartbeat.
func TestHeartbeatAdvertisesV2(t *testing.T) {
	var got int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req HeartbeatReq
		json.NewDecoder(r.Body).Decode(&req)
		got = req.ProtocolVersion
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"shards_assigned":[],"membership_epoch":1}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if _, err := c.Heartbeat(context.Background(), HeartbeatReq{NodeID: "n"}); err != nil {
		t.Fatal(err)
	}
	if got != ProtocolVersion {
		t.Errorf("protocol_version = %d, want %d", got, ProtocolVersion)
	}
}

func TestLeaseUntilParse(t *testing.T) {
	tm, err := LeaseUntil("2026-10-12T12:34:56Z")
	if err != nil {
		t.Fatal(err)
	}
	if tm.Hour() != 12 || tm.Minute() != 34 {
		t.Errorf("bad parse: %v", tm)
	}
	if _, err := LeaseUntil("not-a-time"); err == nil {
		t.Error("expected error for bad timestamp")
	}
}
