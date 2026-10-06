package main

// T4: front/back queue split (Heritrix's key insight).
//
// Separate prioritization from politeness:
//   - Front queues: one per registrable host. Each holds that host's targets
//     in priority order and a next-eligible time. A host's queue only yields
//     its head when the politeness delay has elapsed.
//   - Back queue: picks among eligible hosts by priority (OPIC score order,
//     inherited from the frontier selection).
//
// Before T4, workers blocked sleeping inside hostThrottle/ipThrottle. After,
// the scheduler only hands out targets whose host is already eligible, so
// workers never sleep on politeness. The throttle gates remain as a safety
// net (fast-path no-op when the scheduler did its job).

import (
	"sync"
	"time"
)

// hostFront is one host's front queue.
type hostFront struct {
	queue        []target
	nextEligible time.Time
}

// frontQueue is the back queue: prioritized hosts, each with a front queue.
// Hosts are kept in priority order (the order targets arrived, which is OPIC
// score DESC from the frontier selection).
type frontQueue struct {
	mu    sync.Mutex
	hosts map[string]*hostFront
	order []string // host keys in priority order
	delay time.Duration
}

// newFrontQueue builds a front/back queue from targets, using delay as the
// per-host politeness spacing.
func newFrontQueue(targets []target, delay time.Duration) *frontQueue {
	fq := &frontQueue{
		hosts: make(map[string]*hostFront),
		delay: delay,
	}
	for _, t := range targets {
		fq.add(t)
	}
	return fq
}

// add enqueues a target on its host's front queue, preserving priority order.
func (fq *frontQueue) add(t target) {
	key := hostKey(t.domain)
	fq.mu.Lock()
	defer fq.mu.Unlock()
	h, ok := fq.hosts[key]
	if !ok {
		h = &hostFront{}
		fq.hosts[key] = h
		fq.order = append(fq.order, key)
	}
	h.queue = append(h.queue, t)
}

// next returns the highest-priority target whose host is currently eligible,
// and claims that host (advancing its next-eligible time). ok=false means no
// host is eligible right now (or all queues are drained).
func (fq *frontQueue) next(now time.Time) (t target, ok bool) {
	fq.mu.Lock()
	defer fq.mu.Unlock()
	for _, key := range fq.order {
		h := fq.hosts[key]
		if len(h.queue) == 0 {
			continue
		}
		if now.Before(h.nextEligible) {
			continue
		}
		t = h.queue[0]
		h.queue = h.queue[1:]
		h.nextEligible = now.Add(fq.delay)
		return t, true
	}
	return target{}, false
}

// waitDuration returns how long until the next host becomes eligible, or
// negative if all queues are drained.
func (fq *frontQueue) waitDuration(now time.Time) time.Duration {
	fq.mu.Lock()
	defer fq.mu.Unlock()
	best := time.Duration(-1)
	for _, key := range fq.order {
		h := fq.hosts[key]
		if len(h.queue) == 0 {
			continue
		}
		d := h.nextEligible.Sub(now)
		if d < 0 {
			d = 0
		}
		if best < 0 || d < best {
			best = d
		}
	}
	return best
}

// remaining returns the total number of queued targets.
func (fq *frontQueue) remaining() int {
	fq.mu.Lock()
	defer fq.mu.Unlock()
	n := 0
	for _, key := range fq.order {
		n += len(fq.hosts[key].queue)
	}
	return n
}
