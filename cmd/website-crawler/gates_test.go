package main

// C9: gate-prune adversarial tests. The politeness gates are pruned when idle so
// the maps stay small; a worker holding a gate that gets pruned mid-flight must
// NOT pace on the orphan — it must look the key up again and use the fresh gate.
// Pure in-memory logic: no network, no DB.  go test ./cmd/website-crawler -run TestPrune

import (
	"sync"
	"testing"
	"time"
)

// A gate pruned between batches must not haunt the next batch: after a prune,
// throttle must never sleep on the orphan's stamp — it takes the fresh gate
// (unstamped → no wait).
func TestPruneGatesDeadWorkerUsesFreshGate(t *testing.T) {
	var gates sync.Map

	// Worker loads the gate and stamps it (as throttle does on first use).
	gi, _ := gates.LoadOrStore("example.com", &hostGate{})
	g := gi.(*hostGate)
	g.mu.Lock()
	g.last = time.Now()
	g.mu.Unlock()

	// Pruner runs: the gate is idle far past the tiny test idle, so it is dropped
	// and marked dead.
	pruneGates(&gates, time.Nanosecond)
	if !g.dead {
		t.Fatal("pruned gate must be marked dead")
	}
	if _, ok := gates.Load("example.com"); ok {
		t.Fatal("pruned gate must be removed from the map")
	}

	// The orphan is gone from the map, so throttle takes a fresh gate (unstamped,
	// no wait) instead of sleeping ~300ms on the orphan's stamp.
	start := time.Now()
	throttle(&gates, "example.com", 300*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("throttle slept %v on a pruned orphan gate; must use the fresh gate", elapsed)
	}

	// The fresh gate is now in the map, alive, and stamped.
	gi2, ok := gates.Load("example.com")
	if !ok {
		t.Fatal("throttle must leave a fresh gate in the map")
	}
	g2 := gi2.(*hostGate)
	if g2 == g || g2.dead {
		t.Fatal("fresh gate must be a new, live gate")
	}
	if time.Since(g2.last) > time.Minute {
		t.Fatal("fresh gate must be stamped by throttle")
	}
}

// Deterministic hit of throttle's dead-gate branch: a worker that loaded a gate
// after it was marked dead but before the pruner's delete (the tiny window the
// branch exists for) must skip it and loop to a fresh gate, not pace on it.
func TestThrottleSkipsDeadGateInMap(t *testing.T) {
	var gates sync.Map
	g := &hostGate{last: time.Now(), dead: true} // marked dead, delete not yet run
	gates.Store("doomed.example", g)

	start := time.Now()
	throttle(&gates, "doomed.example", 300*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("throttle slept %v on a dead gate; must skip it", elapsed)
	}
	gi, ok := gates.Load("doomed.example")
	if !ok {
		t.Fatal("throttle must leave a gate in the map")
	}
	if gi.(*hostGate).dead {
		t.Fatal("throttle must replace the dead gate with a live one")
	}
}

// pruneGates must skip a gate that is locked (in use): deleting it under a worker
// would let two workers pace the same host on different gate objects.
func TestPruneGatesSkipsLockedGate(t *testing.T) {
	var gates sync.Map
	g := &hostGate{last: time.Now().Add(-time.Hour)}
	g.mu.Lock()
	gates.Store("locked.example", g)

	pruneGates(&gates, time.Nanosecond)
	g.mu.Unlock()

	if g.dead {
		t.Fatal("a locked (in-use) gate must never be marked dead")
	}
	if _, ok := gates.Load("locked.example"); !ok {
		t.Fatal("a locked gate must stay in the map")
	}
}

// A gate that is idle but still within the idle window survives pruning.
func TestPruneGatesKeepsFreshGate(t *testing.T) {
	var gates sync.Map
	g := &hostGate{last: time.Now()}
	gates.Store("fresh.example", g)

	pruneGates(&gates, time.Hour)

	if g.dead {
		t.Fatal("a recently-used gate must not be pruned")
	}
	if _, ok := gates.Load("fresh.example"); !ok {
		t.Fatal("a recently-used gate must stay in the map")
	}
}

// Adversarial interleaving: pruner and workers racing on the same key must never
// leave a worker pacing on an orphan, and the map must always converge back to
// exactly one live gate per key.
func TestPruneGatesRaceConverges(t *testing.T) {
	var gates sync.Map
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Pruner hammering with a zero idle.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				pruneGates(&gates, 0)
			}
		}
	}()

	// Workers throttling with a microsecond delay (no real sleeping, just gate churn).
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				throttle(&gates, "race.example", time.Microsecond)
			}
		}()
	}
	// Extra workers on distinct keys so the pruner has real work.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			key := string(rune('a'+w)) + ".example"
			for i := 0; i < 100; i++ {
				throttle(&gates, key, time.Microsecond)
			}
		}(w)
	}
	// Let them race briefly, then stop the pruner and drain.
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()

	// After the storm, every key in the map holds a live gate.
	bad := 0
	gates.Range(func(k, v any) bool {
		if v.(*hostGate).dead {
			t.Errorf("key %v left with a dead gate after race", k)
			bad++
		}
		return true
	})
	if bad > 0 {
		t.Fatalf("%d keys with dead gates", bad)
	}
}
