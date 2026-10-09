// Package fleetpace is the seam for fleet-wide crawl politeness (HE-7).
//
// One node is already polite: per-host gates space its own requests. But
// politeness is a fleet property — N independent nodes each respecting a
// host's Crawl-delay still hit that host N× too often. The fix is a shared
// per-host rate limiter, which needs the HE-1 (#135) lease service to host it.
//
// This package defines the interface the crawler paces against
// (FleetPacer), a localPacer adapter over today's single-node gates (what
// ships now), and SimShared — an in-process stand-in for the future central
// bucket that pins the N-node behavior contract in tests until HE-1 lands.
package fleetpace

import (
	"context"
	"sync"
	"time"
)

// FleetPacer paces a node's requests to hosts on behalf of the whole fleet.
type FleetPacer interface {
	// Wait blocks until the caller may make its next request to host.
	// minGap is the host's required inter-request gap: the max of the fleet
	// politeness floor and the host's Crawl-delay. A nil/expired ctx aborts
	// the wait.
	Wait(ctx context.Context, host string, minGap time.Duration) error
	// ReportCrawlDelay publishes a newly learned Crawl-delay for host to the
	// fleet, so nodes that haven't fetched this host's robots.txt yet still
	// pace correctly.
	ReportCrawlDelay(host string, d time.Duration)
}

// gate serializes acquisitions per key with at least gap between them.
// It is the same algorithm as the crawler's hostGate throttle: the first
// acquisition passes immediately, later ones wait out the gap.
type gate struct {
	mu   sync.Mutex
	last time.Time
}

func (g *gate) wait(gap time.Duration) {
	if gap <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if d := gap - time.Since(g.last); d > 0 {
		time.Sleep(d)
	}
	g.last = time.Now()
}

// Local is a FleetPacer backed by per-process gates: today's single-node
// behavior. N processes each running a Local overshoot a host's Crawl-delay
// N× — exactly the problem the shared limiter (post-HE-1) fixes.
type Local struct {
	mu    sync.Mutex
	gates map[string]*gate
}

// NewLocal returns the single-node pacer.
func NewLocal() *Local { return &Local{gates: make(map[string]*gate)} }

func (l *Local) forHost(host string) *gate {
	l.mu.Lock()
	defer l.mu.Unlock()
	g, ok := l.gates[host]
	if !ok {
		g = &gate{}
		l.gates[host] = g
	}
	return g
}

// Wait paces against this process's own gates.
func (l *Local) Wait(ctx context.Context, host string, minGap time.Duration) error {
	if minGap <= 0 {
		return nil
	}
	done := make(chan struct{})
	go func() { l.forHost(host).wait(minGap); close(done) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

// ReportCrawlDelay is a no-op locally: there is no fleet to tell.
func (l *Local) ReportCrawlDelay(host string, d time.Duration) {}

// SimShared is an in-process stand-in for the future central per-host bucket:
// ONE gate map shared by every simulated node, which is exactly what the
// HE-1 lease service will provide. It pins the N-node contract — aggregate
// inter-request gap ≥ the host's gap no matter how many nodes crawl — until
// the real central pacer plugs into the FleetPacer seam.
type SimShared struct {
	mu      sync.Mutex
	gates   map[string]*gate
	learned map[string]time.Duration // Crawl-delays reported via ReportCrawlDelay
}

// NewSimShared returns the shared stand-in pacer.
func NewSimShared() *SimShared {
	return &SimShared{gates: make(map[string]*gate), learned: make(map[string]time.Duration)}
}

// ReportCrawlDelay records the host's delay fleet-wide; later Waits from any
// node use max(minGap, learned).
func (s *SimShared) ReportCrawlDelay(host string, d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if d > s.learned[host] {
		s.learned[host] = d
	}
}

func (s *SimShared) forHost(host string) (*gate, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.gates[host]
	if !ok {
		g = &gate{}
		s.gates[host] = g
	}
	return g, s.learned[host]
}

// Wait paces against the single shared gate for host, honoring the
// fleet-learned Crawl-delay even when the calling node never saw robots.txt.
func (s *SimShared) Wait(ctx context.Context, host string, minGap time.Duration) error {
	g, learned := s.forHost(host)
	if gap := max(minGap, learned); gap > 0 {
		done := make(chan struct{})
		go func() { g.wait(gap); close(done) }()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return nil
		}
	}
	return nil
}

func max(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
