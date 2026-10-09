package main

// HE-7/HE-6 reconciled half-open breaker canary.
//
// A tripped group used to go fully dark for the whole cooldown, then take a
// thundering herd at expiry that usually re-tripped it immediately. Now the
// breaker admits one probe fetch per probe interval while open; a successful
// probe closes the breaker early (fast recovery), a failed probe leaves the
// cooldown UNCHANGED (no ratchet — the HE-6/HE-7 reconcile, per Claude).
//
// Division of labor (the reconcile):
//   - the BREAKER owns WHEN a probe happens: tryProbe admits at most one
//     probe in flight per group, spaced by the bounded interval [30s, 5m];
//   - the FRONTIER RESERVE owns WHICH domain fills the slot: the snapshot
//     draw marks never-crawled candidates (t.probe), and crawlOne only
//     admits reserve-marked probes (admitCanary).

import (
	"log"
	"time"
)

// Half-open probe pacing bounds. A probe is admitted at most once per
// quarter of the group's remaining cooldown, clamped here so sick groups
// aren't probed more than every 30s and recovering groups at most every 5m.
const (
	minProbeInterval = 30 * time.Second
	maxProbeInterval = 5 * time.Minute
)

// tryProbe admits a half-open canary fetch for a breaker-open group. At most
// one probe is in flight per group and probes are spaced at least a quarter
// of the remaining cooldown apart, so a canary never becomes a hammer.
// Reports whether the caller may proceed; the fetch's outcome routes through
// fail()/success(), which apply probe semantics while probeInFlight is set.
func (b *asnBreaker) tryProbe(key string) bool {
	if !b.enabled || key == "" {
		return false
	}
	v, ok := b.m.Load(key)
	if !ok {
		return false
	}
	s := v.(*breakState)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !now.Before(s.openUntil) {
		return false // not open; the normal path handles this fetch
	}
	interval := s.openUntil.Sub(now) / 4
	if interval < minProbeInterval {
		interval = minProbeInterval
	}
	if interval > maxProbeInterval {
		interval = maxProbeInterval
	}
	if s.probeInFlight {
		// A probe is outstanding. If it reported back it cleared this flag;
		// silent for two intervals means it died (panic path) — allow a fresh
		// one rather than wedging the group forever.
		if now.Sub(s.lastProbe) < 2*interval {
			return false
		}
	} else if now.Sub(s.lastProbe) < interval {
		return false // probed recently enough
	}
	s.probeInFlight = true
	s.lastProbe = now
	minc(mBreakerProbes)
	return true
}

// admitCanary reports whether target t may be attempted as the half-open
// canary for its breaker-open group. Only frontier-reserve probes (t.probe)
// are eligible; the breaker owns timing via tryProbe. Callers must only
// call this when breaker.blocked(t.groupKey()).
func admitCanary(t target) bool {
	return t.probe && breaker.tryProbe(t.groupKey())
}

// tripLocked opens the breaker for an escalating cooldown. s.mu must be held.
// It carries the level-clamp the old fail() grew inline: once the cooldown is
// pinned at the cap the level stops ratcheting, so success()'s decay can
// unwind a recovered group in a bounded number of clean cycles.
func (b *asnBreaker) tripLocked(key string, s *breakState, now time.Time, why string) {
	s.level++
	cd := b.base << (s.level - 1)
	if cd > b.cap || cd <= 0 { // cooldown pinned at the cap (or the shift overflowed)
		cd = b.cap
		s.level--
	}
	s.openUntil = now.Add(cd)
	s.fails, s.winStart = 0, time.Time{}
	log.Printf("asn-backoff: OPEN %s for %s (level %d) [%s]", key, cd, s.level, why)
	minc(mBreakerOpen)
}
