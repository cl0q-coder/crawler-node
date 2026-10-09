# HE-6/HE-7 reconciled: one half-open canary

Supersedes #181 (HE-6 frontier sweep) and #187 (HE-7 breaker canary), which
independently added a half-open probe for breaker-open groups with
conflicting semantics. This doc records the reconciled design (per Claude's
2026-10-09 reconcile spec).

## The conflict

- #187 (breaker canary): any blocked target could probe via `tryProbe`; a
  failed probe RE-TRIPPED the breaker with escalation (`tripLocked`).
- #181 (frontier reserve): the snapshot draw marked never-crawled rows as
  probes (`t.probe`); probes bypassed the breaker; `fail()` was assumed to
  never ratchet while open ("probes must not ratchet the cooldown").

Both edited `crawlOne`'s admission path. Merging both as-is would have
produced two probe mechanisms with opposite failure semantics.

## The reconciled rule

**One canary, two roles:**

- The BREAKER owns WHEN a probe happens: `tryProbe` admits at most one
  probe in flight per group, spaced at a quarter of the remaining cooldown
  clamped to [30s, 5m].
- The FRONTIER RESERVE owns WHICH domain fills the slot: the two-pass
  snapshot draw marks never-crawled candidates (`t.probe`, up to
  `probeReserve(n)` slots, `FRONTIER_PROBE_PCT` kill switch, default 1%).

`crawlOne`: `blocked && !(t.probe && tryProbe)` → deferred. Only
reserve-marked probes are eligible; the breaker gates timing.

**Outcome semantics (the crux):**

| probe outcome | reconciled behavior |
|---|---|
| SUCCESS | breaker closes early (fast recovery) — #187's rule kept |
| FAILURE | cooldown UNCHANGED: no extension, no level change — #181's rule kept |

A failed probe clears `probeInFlight` and logs; the group stays open until
`openUntil`, and `tryProbe`'s interval governs the next probe.

## What shipped in the stack

1. Breaker canary core (`he7.go`, `admitCanary`): `tryProbe`, probe-aware
   `fail()`/`success()`, `mBreakerProbes`/`mBreakerProbeOK`.
2. Frontier reserve (`main.go`): two-pass draw, `probeReserve`,
   `FRONTIER_PROBE_PCT`, `capBatch` probe bypass, `sampleUnknown` +
   `mFrontierUnknown`/`mFrontierDrained`/`mFrontierProbed`, drain-rate log.
3. Robots.txt TTL cache (24h, RFC 9309, pruned) + `internal/fleetpace` seam.
4. Tests: `TestReconciledCanaryJoint` (Claude's joint test), reconciled
   breaker tests, ported reserve/draw tests.

## Verification

`go test -race -count=1 ./cmd/website-crawler/ ./internal/fleetpace/` green.
The joint test asserts: exactly one probe per interval from the reserve;
success closes early; three consecutive failures leave `openUntil` and
`level` bit-identical; no double-probe under race (32 goroutines → 1).

## Sequence

#164 (HE-4) → this PR → rest. #181 and #187 closed as superseded.
