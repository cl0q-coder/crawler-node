package main

// Tests for the O6 DNS cache (dnscache.go).

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"cl0q/netguard"
)

func stubLookup(addrs []net.IPAddr, err error) (func(context.Context, string) ([]net.IPAddr, error), *int) {
	calls := 0
	return func(ctx context.Context, host string) ([]net.IPAddr, error) {
		calls++
		return addrs, err
	}, &calls
}

var stubAddrs = []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}, {IP: net.ParseIP("2606:2800:220:1:248:1893:25c8:1946")}}

func TestDNSCacheHit(t *testing.T) {
	lookup, calls := stubLookup(stubAddrs, nil)
	c := newDNSCache(time.Minute, lookup)
	ctx := context.Background()
	a1, err := c.lookupIPAddr(ctx, "Example.COM")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := c.lookupIPAddr(ctx, "example.com") // different case, same key
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatalf("underlying lookup called %d times, want 1 (second was a hit)", *calls)
	}
	if len(a1) != len(a2) || a1[0].IP.String() != a2[0].IP.String() {
		t.Fatalf("hit returned different addresses: %v vs %v", a1, a2)
	}
}

func TestDNSCacheExpiry(t *testing.T) {
	lookup, calls := stubLookup(stubAddrs, nil)
	c := newDNSCache(20*time.Millisecond, lookup)
	ctx := context.Background()
	if _, err := c.lookupIPAddr(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if _, err := c.lookupIPAddr(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Fatalf("underlying lookup called %d times, want 2 (entry expired)", *calls)
	}
}

func TestDNSCacheFailureNotCached(t *testing.T) {
	lookup, calls := stubLookup(nil, errors.New("no such host"))
	c := newDNSCache(time.Minute, lookup)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := c.lookupIPAddr(ctx, "example.com"); err == nil {
			t.Fatal("want error")
		}
	}
	if *calls != 2 {
		t.Fatalf("failures must not be cached: lookup called %d times, want 2", *calls)
	}
	if c.size() != 0 {
		t.Fatalf("failed lookup left %d cache entries, want 0", c.size())
	}
}

func TestDNSCacheEvictsExpiredAtCap(t *testing.T) {
	lookup, _ := stubLookup(stubAddrs, nil)
	c := newDNSCache(time.Minute, lookup)
	// Fill with expired entries by hand (same package).
	past := time.Now().Add(-time.Hour)
	c.mu.Lock()
	for i := 0; i < dnsCacheMaxEntries; i++ {
		c.m[string(rune('a'+i%26))+string(rune('0'+i/26%10))+string(rune(i))] = dnsCacheEntry{addrs: stubAddrs, expires: past}
	}
	c.mu.Unlock()
	if _, err := c.lookupIPAddr(context.Background(), "fresh.example.com"); err != nil {
		t.Fatal(err)
	}
	if got := c.size(); got > dnsCacheMaxEntries {
		t.Fatalf("cache has %d entries, over cap %d", got, dnsCacheMaxEntries)
	}
	if _, ok := c.m["fresh.example.com"]; !ok {
		t.Fatal("fresh entry missing after eviction pass")
	}
}

// withTestDNS swaps the global dnsAnswers for the test and restores it after.
func withTestDNS(t *testing.T, c *dnsCache) {
	t.Helper()
	old := dnsAnswers
	dnsAnswers = c
	t.Cleanup(func() { dnsAnswers = old })
}

func withPublicDial(t *testing.T) {
	t.Helper()
	netguard.SetAllowNonPublicForTest(true)

	t.Cleanup(func() { netguard.SetAllowNonPublicForTest(true) })
}

func TestDialCachedIPLiteralSkipsLookup(t *testing.T) {
	withPublicDial(t)
	lookup, calls := stubLookup(nil, errors.New("must not be called"))
	withTestDNS(t, newDNSCache(time.Minute, lookup))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	conn, err := dialCached(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if *calls != 0 {
		t.Fatalf("IP literal triggered %d DNS lookups, want 0", *calls)
	}
}

func TestDialCachedFallsBackAcrossAnswers(t *testing.T) {
	withPublicDial(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	// First answer refuses (nothing on 127.0.0.2), second connects: the dial
	// must fall back across the answer list like net.Dialer does.
	lookup, _ := stubLookup([]net.IPAddr{{IP: net.ParseIP("127.0.0.2")}, {IP: net.ParseIP("127.0.0.1")}}, nil)
	withTestDNS(t, newDNSCache(time.Minute, lookup))
	conn, err := dialCached(context.Background(), "tcp", "fakehost.invalid:"+port)
	if err != nil {
		t.Fatalf("dialCached did not fall back to the second answer: %v", err)
	}
	conn.Close()
}

func TestDialCachedEvictsOnTotalFailure(t *testing.T) {
	withPublicDial(t)
	lookup, calls := stubLookup([]net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil)
	withTestDNS(t, newDNSCache(time.Minute, lookup))

	// Closed port: every cached address fails → error + eviction.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	if _, err := dialCached(context.Background(), "tcp", "fakehost.invalid:"+mustPort(addr)); err == nil {
		t.Fatal("want dial error for closed port")
	}
	if *calls != 1 {
		t.Fatalf("lookup called %d times, want 1", *calls)
	}
	// Entry was evicted on total failure: the next dial re-resolves.
	if _, err := dialCached(context.Background(), "tcp", "fakehost.invalid:"+mustPort(addr)); err == nil {
		t.Fatal("want dial error for closed port")
	}
	if *calls != 2 {
		t.Fatalf("stale entry was not evicted: lookup called %d times, want 2", *calls)
	}
}

func mustPort(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		panic(err)
	}
	return port
}

func BenchmarkDNSCacheHit(b *testing.B) {
	lookup, _ := stubLookup(stubAddrs, nil)
	c := newDNSCache(time.Minute, lookup)
	ctx := context.Background()
	if _, err := c.lookupIPAddr(ctx, "example.com"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.lookupIPAddr(ctx, "example.com")
	}
}
