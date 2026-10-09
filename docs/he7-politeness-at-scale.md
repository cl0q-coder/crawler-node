# HE-7: Crawl politeness at scale (fleet-wide rate limiting)

Status: design + single-node implementation shipped. Fleet-coordination parts are
**BLOCKED on HE-1 (#135)** — Claude Code's agreement on the PROTOCOL v2
claim/heartbeat wire — and are designed here, not built.

## Problem

One node is already polite: per-host gates space requests ≥1s apart per eTLD+1
(and per server IP), `robotsFor` single-flights robots.txt, and `sitePace` honors
`max(hostDelay, Crawl-delay)`. But politeness is a *fleet* property. N nodes
crawling independently each respect a host's Crawl-delay D, and the host still
sees requests every D/N — N nodes behind one name is a DDoS with our name on it.

## Goals

1. A host must never see the fleet exceed its Crawl-delay (or our 1s floor),
   no matter how many nodes are crawling.
2. robots.txt is fetched once per TTL fleet-wide, not once per node per batch.
3. Breaker trips degrade gracefully: probes keep the frontier moving and detect
   recovery quickly, instead of full-dark cooldowns followed by a thundering herd.

## Design

### 1. Fleet per-host rate limiting — BLOCKED on HE-1 (#135)

Today each node owns a `hostGates` map and paces locally. The fleet design moves
the gate to the HE-1 lease service:

- The lease service holds one token bucket per registrable host: capacity 1,
  refill interval = `max(fleet floor 1s, host's Crawl-delay)`.
- Before a node fetches from a host it claims a pacing slot (claim = "pace
  <host>"). The claim is cheap and local-first: the service returns either
  GO (slot granted) or WAIT <ms> (next refill).
- The wire rides HE-1's PROTOCOL v2 heartbeat channel so no new connection is
  needed; pacing claims piggyback on the existing heartbeat round-trip, and the
  node's claim is implied by its shard assignment while the fleet is small.
- Fallback: if the lease service is unreachable, the node reverts to local
  pacing (`localPacer`) — never to unpaced fetching. Degraded politeness (N×
  overshoot possible) beats a stalled fleet, and the outage is metric-visible.

The seam exists now: `internal/fleetpace.FleetPacer` (`Wait`/`ReportCrawlDelay`)
with a `localPacer` adapter over today's gates. The central implementation plugs
in post-HE-1 without touching crawlOne.

### 2. Shared robots.txt cache — BLOCKED on HE-1 (#135)

- The lease service (or a small sidecar on it) keeps the fleet robots cache:
  host → {verdict, Crawl-delay, rules hash, fetched-at, TTL 24h}.
- Nodes keep their local TTL cache (shipped in HE-7, §4) and consult the shared
  cache on miss before fetching. A robots.txt is fetched at most once per TTL
  fleet-wide; the fetching node publishes the verdict.
- Crawl-delay changes propagate: when a node learns a new delay it reports it
  (`FleetPacer.ReportCrawlDelay`), the shared cache updates, and the central
  bucket's refill interval follows. No node needs to re-fetch to learn.
- Poisoning guard: only verdicts fetched by enrolled, non-quarantined nodes are
  cached fleet-wide (ties into the node-trust work, G-12).

### 3. Breaker graceful degradation — SHIPPED (single-node)

HE-6's probe reserve keeps breaker-open groups in the frontier draw. HE-7
extends the breaker itself with a half-open canary:

- While a group is open, one probe fetch is admitted per probe interval
  (`breakProbeInterval`, default 1/4 of the group's current cooldown, min 30s).
- Probe success → the group recovers immediately: breaker closes, escalation
  level decays — no waiting out the full cooldown.
- Probe failure → the breaker re-opens and the escalation level ratchets as
  before.
- At most one probe is in flight per group; probes are paced through the normal
  host/IP gates, so a canary never becomes a hammer.

This replaces the old shape (full-dark cooldown → thundering herd at expiry →
usually an immediate re-trip) with continuous low-rate probing and fast
recovery. Metric: `crawler_breaker_probes_total` (attempts) and probe outcomes
visible via the existing `crawler_breaker_open_groups` gauge dropping early.

### 4. robots.txt cache TTL — SHIPPED (single-node)

`robotsCache` was wiped every batch cycle, so every node refetched every host's
robots.txt every cycle. Now entries carry a 24h TTL (RFC 9309's recommended
upper bound for robots caching); `robotsFor` serves fresh entries and refetches
expired ones; a periodic prune drops expired entries so the map stays bounded
(the 2026-07-13 leak class stays fixed). Single-flight across concurrent
callers is unchanged. When the shared cache (§2) lands, the TTL layer is what
it backs.

### 5. N-node behavior contract

With the fleet coordinator in place, N nodes crawling the same host with
Crawl-delay D must produce aggregate inter-request gaps ≥ D. The simulation in
`internal/fleetpace` (`simSharedPacer`: one shared gate standing in for the
future central bucket) pins this contract today; the same test against N
independent `localPacer`s demonstrates the N× overshoot HE-7 eliminates.

## Metrics

- `crawler_breaker_probes_total` — half-open probe attempts (new).
- `crawler_breaker_probe_success_total` — probes that recovered a group (new).
- `crawler_breaker_opens_total`, `crawler_breaker_open_groups` — existing.
- Fleet pacing: per-host claim WAIT rates (post-HE-1; defined here, implemented
  with the central pacer).

## Rollout

1. HE-7 (this): half-open breaker, robots TTL cache, `fleetpace` seam + sim.
2. HE-1 lands → central pacer + shared robots cache plug into the seam.
3. Measure fleet aggregate pacing against top-100 hosts before/after; tune the
   heartbeat piggyback interval so pacing claims don't inflate heartbeat load.
