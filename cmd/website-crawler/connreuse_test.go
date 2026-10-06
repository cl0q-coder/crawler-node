package main

// O7: connection-reuse verification. The production transport is configured
// for keep-alive (MaxIdleConns/PerHost, 30s idle); these tests prove requests
// through the REAL transport (real dialCached, real DNS cache, real
// netguard.Control) actually reuse TCP connections when bodies are drained
// the way fetchPage/fetchRobots drain them — and that oversized bodies
// intentionally do NOT reuse (the 2MiB/1MiB caps trade reuse for a memory
// bound; draining unbounded would let one hostile server wedge a worker).

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// stubLoopbackDNS resolves every host to 127.0.0.1 for reuse tests.
func stubLoopbackDNS(t *testing.T) {
	t.Helper()
	lookup := func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	withTestDNS(t, newDNSCache(time.Minute, lookup))
}

// reuseServer starts a server counting accepted TCP connections and returns
// its fake-host URL (resolved to 127.0.0.1 by stubLoopbackDNS) plus a conns
// counter.
func reuseServer(t *testing.T, handler http.HandlerFunc) (url string, conns *int64) {
	t.Helper()
	conns = new(int64)
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			atomic.AddInt64(conns, 1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	return "http://reuse.test:" + port, conns
}

// fetchLikePage performs a GET and drains the body exactly the way fetchPage
// does (LimitReader cap + full read + close), so the connection is reusable.
func fetchLikePage(t *testing.T, client *http.Client, url string, cap int64) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_, _ = io.ReadAll(io.LimitReader(resp.Body, cap))
	resp.Body.Close()
}

func TestTransportReusesConnection(t *testing.T) {
	withPublicDial(t) // dials 127.0.0.1 via stub DNS
	stubLoopbackDNS(t)
	const n = 20
	url, conns := reuseServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><head><title>t</title></head><body>hello</body></html>")
	})
	client := &http.Client{Transport: transport}
	for i := 0; i < n; i++ {
		fetchLikePage(t, client, url, 2<<20)
	}
	if got := atomic.LoadInt64(conns); got != 1 {
		t.Fatalf("%d requests opened %d TCP connections, want 1 (keep-alive reuse)", n, got)
	}
}

func TestTransportReusesAcrossHosts(t *testing.T) {
	withPublicDial(t)
	stubLoopbackDNS(t)
	// Two distinct fake hostnames resolving to the same server: the transport
	// keys idle connections by host, so expect one connection PER host.
	base, conns := reuseServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	_, port, _ := net.SplitHostPort(base[len("http://"):])
	urlA := "http://host-a.test:" + port
	urlB := "http://host-b.test:" + port
	client := &http.Client{Transport: transport}
	for i := 0; i < 10; i++ {
		fetchLikePage(t, client, urlA, 2<<20)
		fetchLikePage(t, client, urlB, 2<<20)
	}
	if got := atomic.LoadInt64(conns); got != 2 {
		t.Fatalf("20 requests to 2 hosts opened %d TCP connections, want 2 (one reused conn per host)", got)
	}
}

func TestOversizedBodyDoesNotReuse(t *testing.T) {
	withPublicDial(t)
	stubLoopbackDNS(t)
	big := make([]byte, 3<<20) // over the 2MiB page cap
	url, conns := reuseServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(big)
	})
	client := &http.Client{Transport: transport}
	// Same drain pattern as fetchPage: cap 2MiB, so 1MiB stays unread.
	fetchLikePage(t, client, url, 2<<20)
	fetchLikePage(t, client, url, 2<<20)
	if got := atomic.LoadInt64(conns); got != 2 {
		t.Fatalf("oversized bodies opened %d connections, want 2 (no reuse past the drain cap, by design)", got)
	}
}
