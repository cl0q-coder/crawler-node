package main

// DNS caching for the crawl hot path.
//
// Go's net.Resolver does no caching: every DialContext to a new hostname paid
// a TCP DNS round-trip to 1.1.1.1 (A + AAAA) — while holding dialSem — and
// lookupIP (pacing) paid it again per fetch. Hosts are re-contacted within
// seconds (robots.txt then page, redirect chains, politeness-gated revisits),
// so a short cache collapses the repeats.
//
// The TTL is fixed (see newDNSCache), not per-record: the stdlib resolver
// doesn't expose record TTLs, and parsing them would need a DNS library on the
// security-critical dial path. The default 60s bounds staleness; a changed A
// record is picked up at most 60s later, and a total dial failure evicts the
// entry immediately so the next dial re-resolves fresh.
//
// Failures are never cached. The dial path tries every cached address in turn
// — the dialer's own cross-answer fallback, minus its Happy-Eyeballs racing
// (sequential is fine, and more polite, for a crawler) — so netguard.Control
// still sees, and can still refuse, each actual connection attempt. Dialing by
// IP is transparent to TLS: SNI, certificate verification and the Host header
// keep using the URL host.

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	// dnsCacheTTL bounds how stale a cached answer can be.
	dnsCacheTTL = 60 * time.Second
	// dnsCacheMaxEntries bounds cache memory; eviction prefers expired entries.
	dnsCacheMaxEntries = 10000
)

type dnsCacheEntry struct {
	addrs   []net.IPAddr
	expires time.Time
}

// dnsCache is a fixed-TTL cache over a lookup function (normally
// resolver.LookupIPAddr). The function type keeps it unit-testable without a
// DNS server.
type dnsCache struct {
	mu     sync.Mutex
	m      map[string]dnsCacheEntry
	ttl    time.Duration
	lookup func(ctx context.Context, host string) ([]net.IPAddr, error)
}

func newDNSCache(ttl time.Duration, lookup func(ctx context.Context, host string) ([]net.IPAddr, error)) *dnsCache {
	return &dnsCache{m: make(map[string]dnsCacheEntry), ttl: ttl, lookup: lookup}
}

// lookupIPAddr returns the cached addresses when fresh, resolving and caching
// on a miss. Lookup failures are never cached.
func (c *dnsCache) lookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	key := strings.ToLower(host)
	now := time.Now()
	c.mu.Lock()
	if e, ok := c.m[key]; ok && now.Before(e.expires) {
		addrs := e.addrs
		c.mu.Unlock()
		return addrs, nil
	}
	c.mu.Unlock()

	addrs, err := c.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	cp := make([]net.IPAddr, len(addrs))
	copy(cp, addrs)
	c.mu.Lock()
	if len(c.m) >= dnsCacheMaxEntries {
		for h, e := range c.m {
			if !now.Before(e.expires) {
				delete(c.m, h)
			}
		}
		if len(c.m) >= dnsCacheMaxEntries {
			// Everything is fresh and we're still over cap: drop one
			// arbitrary entry to bound memory.
			for h := range c.m {
				delete(c.m, h)
				break
			}
		}
	}
	c.m[key] = dnsCacheEntry{addrs: cp, expires: now.Add(c.ttl)}
	c.mu.Unlock()
	return addrs, nil
}

// evict drops host from the cache, forcing a fresh resolve on next use.
// Called when every cached address failed to dial (likely stale).
func (c *dnsCache) evict(host string) {
	c.mu.Lock()
	delete(c.m, strings.ToLower(host))
	c.mu.Unlock()
}

// size reports the entry count (for tests).
func (c *dnsCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// dnsAnswers caches resolver lookups for the dial hot path and for pacing.
var dnsAnswers = newDNSCache(dnsCacheTTL, func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return resolver.LookupIPAddr(ctx, host)
})

// dialCached resolves host through the DNS cache and dials each answer in
// turn until one connects, like net.Dialer's own cross-answer fallback.
// IP literals skip the lookup entirely.
func dialCached(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip != nil {
		return ipDialer.DialContext(ctx, network, addr)
	}
	addrs, err := dnsAnswers.lookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var firstErr error
	dialed := 0
	for _, ia := range addrs {
		if network == "tcp4" && ia.IP.To4() == nil {
			continue
		}
		if network == "tcp6" && ia.IP.To4() != nil {
			continue
		}
		dialed++
		conn, err := ipDialer.DialContext(ctx, network, net.JoinHostPort(ia.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if dialed == 0 {
		return nil, fmt.Errorf("dns: no %s addresses for %s", network, host)
	}
	// Every cached address failed: likely stale — evict so the next dial
	// re-resolves fresh instead of retrying dead IPs for the rest of the TTL.
	dnsAnswers.evict(host)
	return nil, firstErr
}
