// PROTOCOL v2 wire types and HTTP client (HE-1, issue #135).
//
// Additive wire: the client never sets DisallowUnknownFields, so the server
// can add fields without breaking older clients; the server ignores unknown
// fields it doesn't know. Nodes advertise protocol_version in enroll +
// heartbeat; v1 nodes keep working standalone.
package he1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// NodeMetrics is the per-heartbeat telemetry the node reports.
type NodeMetrics struct {
	PagesLast30s  int    `json:"pages_last_30s"`
	P99CrawlMs    int    `json:"p99_crawl_ms"`
	ErrorsLast30s int    `json:"errors_last_30s"`
	Version       string `json:"version"`
}

// PushedConfig is the central's config push inside the heartbeat response.
// Zero/negative values mean "unset" — see ApplyPushedFloors.
type PushedConfig struct {
	MinHostGapMs         int `json:"min_host_gap_ms"`
	MaxPagesPerHostCycle int `json:"max_pages_per_host_cycle"`
	RecrawlDays          int `json:"recrawl_days"`
	HeartbeatIntervalS   int `json:"heartbeat_interval_s"`
}

// HeartbeatReq is POST /api/nodes/heartbeat (every 30s, node-token auth).
type HeartbeatReq struct {
	NodeID          string      `json:"node_id"`
	ProtocolVersion int         `json:"protocol_version"`
	ShardsHeld      []uint16    `json:"shards_held"`
	InFlightCount   int         `json:"in_flight_count"`
	LeaseRenewals   []string    `json:"lease_renewals"`
	Metrics         NodeMetrics `json:"metrics"`
}

// HeartbeatResp answers the heartbeat: new shard assignment, lease renewal
// timestamps, pushed config, and the membership epoch (bumps on ring change;
// the node re-filters its draw when it moves).
type HeartbeatResp struct {
	ShardsAssigned    []uint16          `json:"shards_assigned"`
	LeaseRenewedUntil map[string]string `json:"lease_renewed_until"`
	Config            PushedConfig      `json:"config"`
	MembershipEpoch   int               `json:"membership_epoch"`
}

// ClaimReq is POST /api/frontier/claim (node-token auth).
type ClaimReq struct {
	NodeID string   `json:"node_id"`
	Count  int      `json:"count"`
	Shards []uint16 `json:"shards"`
}

// ClaimedDomain is one domain under lease.
type ClaimedDomain struct {
	ID     string `json:"id"`
	Domain string `json:"domain"`
	Shard  uint16 `json:"shard"`
}

// ClaimResp answers the claim: the lease id, its deadline (RFC3339), and the
// leased domains. lease_until = 2× node p99 batch latency, floored 5min,
// capped 30min, 10min default on first claim pre-p99.
type ClaimResp struct {
	LeaseID    string          `json:"lease_id"`
	LeaseUntil string          `json:"lease_until"`
	Domains    []ClaimedDomain `json:"domains"`
}

// ReleaseReq is the optional POST /api/frontier/release (skip-decided
// domains; low priority — ingest implicitly releases anyway).
type ReleaseReq struct {
	NodeID    string   `json:"node_id"`
	DomainIDs []string `json:"domain_ids"`
}

// MaxClaimCount is the central-side cap on a single claim (wire §2).
const MaxClaimCount = 2000

// Client speaks the v2 coordination endpoints against an ingest base URL.
// Auth is the node token (Bearer); the HTTP client never follows redirects
// (PROTOCOL.md §2) so the token only ever goes to the ingest URL.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient builds a v2 client for ingestURL (e.g. https://cl0q.com).
func NewClient(ingestURL, token string) *Client {
	return &Client{
		baseURL: ingestURL,
		token:   token,
		http: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return fmt.Errorf("he1: refusing redirect to %s", req.URL)
			},
		},
	}
}

// Heartbeat posts the node heartbeat and decodes the central's response.
func (c *Client) Heartbeat(ctx context.Context, req HeartbeatReq) (HeartbeatResp, error) {
	req.ProtocolVersion = ProtocolVersion
	var resp HeartbeatResp
	if err := c.post(ctx, "/api/nodes/heartbeat", req, &resp); err != nil {
		return HeartbeatResp{}, err
	}
	return resp, nil
}

// Claim requests up to count domains from the given shards.
func (c *Client) Claim(ctx context.Context, req ClaimReq) (ClaimResp, error) {
	if req.Count > MaxClaimCount {
		req.Count = MaxClaimCount
	}
	var resp ClaimResp
	if err := c.post(ctx, "/api/frontier/claim", req, &resp); err != nil {
		return ClaimResp{}, err
	}
	return resp, nil
}

// Release tells the central the node decided to skip these domains.
func (c *Client) Release(ctx context.Context, req ReleaseReq) error {
	return c.post(ctx, "/api/frontier/release", req, nil)
}

// LeaseUntil parses a claim/heartbeat lease timestamp (RFC3339).
func LeaseUntil(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}

func (c *Client) post(ctx context.Context, path string, req any, resp any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("he1: marshal %s: %w", path, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("he1: build %s: %w", path, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("he1: POST %s: %w", path, err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return fmt.Errorf("he1: POST %s: status %d", path, httpResp.StatusCode)
	}
	if resp == nil {
		return nil
	}
	// Additive wire: unknown fields are ignored, never rejected.
	if err := json.NewDecoder(httpResp.Body).Decode(resp); err != nil {
		return fmt.Errorf("he1: decode %s: %w", path, err)
	}
	return nil
}
