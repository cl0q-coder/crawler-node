package main

import (
	"testing"
	"time"
)

func TestFrontQueuePriorityOrder(t *testing.T) {
	// Targets arrive in priority order (OPIC DESC). The queue must hand them
	// out eligible-first, preserving priority among eligible hosts.
	now := time.Now()
	fq := newFrontQueue([]target{
		{domain: "a.com"},
		{domain: "b.com"},
		{domain: "a.com"}, // second URL on a.com
	}, 5*time.Second)

	// First: a.com (highest priority, eligible).
	t1, ok := fq.next(now)
	if !ok || t1.domain != "a.com" {
		t.Fatalf("first = %v, %v; want a.com, true", t1.domain, ok)
	}
	// a.com is now ineligible for 5s; next should be b.com.
	t2, ok := fq.next(now)
	if !ok || t2.domain != "b.com" {
		t.Fatalf("second = %v, %v; want b.com, true", t2.domain, ok)
	}
	// Both a.com and b.com ineligible now; nothing eligible.
	if _, ok := fq.next(now); ok {
		t.Fatal("expected no eligible host")
	}
	// After 5s, a.com's second URL becomes eligible (priority over nothing else).
	t3, ok := fq.next(now.Add(6 * time.Second))
	if !ok || t3.domain != "a.com" {
		t.Fatalf("third = %v, %v; want a.com, true", t3.domain, ok)
	}
}

func TestFrontQueueWaitDuration(t *testing.T) {
	now := time.Now()
	fq := newFrontQueue([]target{{domain: "a.com"}, {domain: "a.com"}, {domain: "b.com"}}, 10*time.Second)
	if _, ok := fq.next(now); !ok {
		t.Fatal("expected a target")
	}
	// a.com claimed for 10s (one URL remains queued); b.com still eligible → wait 0.
	if w := fq.waitDuration(now); w != 0 {
		t.Errorf("wait = %v, want 0 (b.com eligible)", w)
	}
	if _, ok := fq.next(now); !ok {
		t.Fatal("expected b.com")
	}
	// b.com drained; a.com's second URL waits ~10s.
	if w := fq.waitDuration(now); w < 9*time.Second || w > 10*time.Second {
		t.Errorf("wait = %v, want ~10s", w)
	}
}

func TestFrontQueueDrained(t *testing.T) {
	fq := newFrontQueue([]target{{domain: "a.com"}}, time.Second)
	now := time.Now()
	if _, ok := fq.next(now); !ok {
		t.Fatal("expected a target")
	}
	if _, ok := fq.next(now.Add(2 * time.Second)); ok {
		t.Fatal("queue should be drained")
	}
	if w := fq.waitDuration(now); w >= 0 {
		t.Errorf("wait = %v, want negative (drained)", w)
	}
	if n := fq.remaining(); n != 0 {
		t.Errorf("remaining = %d, want 0", n)
	}
}

func TestFrontQueueGrouping(t *testing.T) {
	// Multiple targets on the same registrable host share one front queue.
	fq := newFrontQueue([]target{
		{domain: "www.a.com"},
		{domain: "blog.a.com"},
		{domain: "b.com"},
	}, 5*time.Second)
	if n := fq.remaining(); n != 3 {
		t.Fatalf("remaining = %d, want 3", n)
	}
	// www.a.com and blog.a.com should map to the same host key (registrable).
	now := time.Now()
	t1, _ := fq.next(now)
	t2, _ := fq.next(now)
	// One of t1/t2 is a.com-family, the other is b.com; the second a.com URL
	// must NOT come out before the delay.
	t3, ok := fq.next(now)
	if ok {
		t.Fatalf("third target %q came out without delay; politeness violated", t3.domain)
	}
	_ = t1
	_ = t2
}

func BenchmarkFrontQueueNext(b *testing.B) {
	var targets []target
	for i := 0; i < 1000; i++ {
		targets = append(targets, target{domain: string(rune('a'+i%26)) + ".example.com"})
	}
	fq := newFrontQueue(targets, time.Millisecond)
	now := time.Now()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		fq.next(now.Add(time.Duration(i) * time.Millisecond))
	}
}
