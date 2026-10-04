package main

// Benchmarks for the O2 (HTML parse) and O3 (frontier + gates) optimizations.
// Run: go test -run xxx -bench 'BenchmarkParseHTMLInfo|BenchmarkCapBatch|BenchmarkThrottleHit|BenchmarkPruneGates100k' -benchmem ./cmd/website-crawler/

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/html"
)

// BenchmarkParseHTMLInfo runs the tokenizer loop over small, hand-written
// homepage fixtures (testdata/homepages/*.html). The DOM is parsed once per
// fixture outside the timer; the loop measures parseHTMLInfo only.
func BenchmarkParseHTMLInfo(b *testing.B) {
	old := crawlSubdomains.Load()
	crawlSubdomains.Store(false)
	b.Cleanup(func() { crawlSubdomains.Store(old) })

	files, err := filepath.Glob("testdata/homepages/*.html")
	if err != nil || len(files) == 0 {
		b.Fatal("no fixtures in testdata/homepages")
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			b.Fatal(err)
		}
		doc, err := html.Parse(strings.NewReader(string(raw)))
		if err != nil {
			b.Fatal(err)
		}
		// Fail fast if a fixture exercises nothing: a benchmark that parses
		// zero words would silently measure an empty loop.
		probe := &WebsiteInfo{}
		parseHTMLInfo(doc, probe, "example.com")
		if probe.WordCount == 0 || probe.Title == "" {
			b.Fatalf("%s: fixture yielded no words/title; refusing to bench nothing", f)
		}
		b.Run(filepath.Base(f), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				parseHTMLInfo(doc, &WebsiteInfo{}, "example.com")
			}
		})
	}
}

// benchCapBatchTargets builds a deterministic pool: some targets share an ASN
// org, some an IP, the rest fall back to their registrable parent; domains
// repeat so the dedup path is exercised too.
func benchCapBatchTargets(n int) []target {
	orgs := []string{"Example Hosting", "Another Network", "Third ASN"}
	ts := make([]target, 0, n)
	for i := 0; i < n; i++ {
		t := target{domain: fmt.Sprintf("host%d.example%d.com", i%2000, i%97)}
		switch i % 5 {
		case 0, 1:
			t.org = orgs[(i/5)%len(orgs)]
		case 2:
			t.ip = fmt.Sprintf("203.0.113.%d", i%251)
		}
		ts = append(ts, t)
	}
	return ts
}

// BenchmarkCapBatch measures the per-batch selection cost over a 20k pool
// capped to 20/group and 1000 total. capBatch logs once per call, so the log
// is discarded for the run.
func BenchmarkCapBatch(b *testing.B) {
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })
	ts := benchCapBatchTargets(20000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := capBatch(ts, 20, 1000)
		if len(out) == 0 {
			b.Fatal("capBatch returned nothing")
		}
	}
}

// BenchmarkThrottleHit measures the uncontended throttle path: the key exists,
// its last stamp is old, so no wait happens. Keys are pre-generated (slicing
// the slice is alloc-free) and the pool is large enough that the stamped gap
// always exceeds the microsecond delay — no iteration ever sleeps. Before the
// Load fast-path fix this showed 1 alloc/op (the discarded &hostGate{});
// after the fix it must show 0.
func BenchmarkThrottleHit(b *testing.B) {
	var gates sync.Map
	const keys = 4096
	keyStrs := make([]string, keys)
	old := time.Now().Add(-time.Hour)
	for i := range keyStrs {
		keyStrs[i] = fmt.Sprintf("throttle-bench-%d", i)
		gates.Store(keyStrs[i], &hostGate{last: old})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		throttle(&gates, keyStrs[i%keys], time.Microsecond)
	}
}

// BenchmarkPruneGates100k measures pruning a full 100k-entry gate map. The
// refill runs with the timer stopped so only the Range/TryLock/delete pass is
// measured. Every gate is idle, so the map must be empty afterwards.
func BenchmarkPruneGates100k(b *testing.B) {
	gates := &sync.Map{}
	refill := func() {
		old := time.Now().Add(-time.Hour)
		for i := 0; i < 100000; i++ {
			gates.Store(i, &hostGate{last: old})
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		refill()
		b.StartTimer()
		pruneGates(gates, gateIdle)
		if i == 0 {
			n := 0
			gates.Range(func(_, _ any) bool { n++; return true })
			if n != 0 {
				b.Fatalf("pruneGates left %d gates; want 0", n)
			}
		}
	}
}
