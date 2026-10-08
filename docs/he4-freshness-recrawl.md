# HE-4: Freshness + Recrawl Scheduling — Design Note v1

**Status:** §§1–6 implemented 2026-10-07 (branch `dutch/he4-freshness-recrawl`,
PR #164) — see "Implementation" below. §7 still provisional (needs #135).
**Author:** Dutch, 2026-10-07
**Issue:** crawler-node #146. **Extends:** HE-1 distributed crawl coordination (#135).
**Dependency note:** §7 (fleet scheduling) rides the HE-1 lease+heartbeat wire.
#135 is still awaiting Claude Code's PROTOCOL v2 agreement, so §7 is
**provisional**. §§1–6 are single-node and have no HE-1 dependency.

## Implementation (2026-10-07)

Shipped on `dutch/he4-freshness-recrawl` (PR crawler-node #164), all per the
spec above:

- **Schema** — `migrations/002_he4_freshness.sql`: `change_rate REAL`,
  `next_score REAL`, `sitemap_lastmod TIMESTAMPTZ` (all NULL-able, lazily
  backfilled) + partial btree `idx_domain_info_next_score WHERE next_score
  IS NOT NULL`. Columns also ensured at startup (`ensureHe4Columns`).
- **Signals** — `he4FetchVerdicts` reads stored SimHashes before the
  website_info upsert; delta ≥8 bits = changed. 304s keep their verdict.
  `sitemap_lastmod` persisted as a monotonic max on domain_info.
- **Scoring** — `he4PriorityScore` = importance × (1-exp(-λ·age)) ×
  (age/interval) × 2.0 sitemap boost; `change_rate` EWMA α=0.3 clamped to
  [1/365, 2.0]; λ-aware T3 factors (tighten 0.5+0.25e^-λ, backoff
  1.1+0.15(1-e^-λ)), driven by the SimHash verdict.
- **Queue** — `selectFromSnapshot` draws `WHERE next_score IS NOT NULL ORDER
  BY next_score DESC` (index range scan) with fallback to the legacy
  unordered draw; `refreshRecrawlScores` sweeper (15-min cadence +
  in-transaction clearing of just-crawled domains).
- **Simulation** — `internal/recrawlsim`: (d) cuts importance-weighted lag
  **67%** vs (a) at 800 fetches/day (0.59 vs 1.77 days), max overdue 1.97
  (< 2×) in every importance decile — the §8 bar passes. Bonus: legacy T3's
  validator-blind 200-tightening more than doubles crawl load (2297 vs 1116
  fetches/day); SimHash verdicts fix it.
- **Benchmarks** — Go-side scoring is 0.6–23 ns/op, zero allocs; the draw is
  an index scan (EXPLAIN on prod-scale data is a deploy-time step).

**Follow-up (do not block review):** the §8 sim exposed that the specified
binary-observation EWMA converges to P(change per fetch), not changes/day —
expected_gain is miscalibrated badly enough that the priority draw alone (b)
underperforms uniform (2.75 vs 1.77d lag). An interval-censored Poisson MLE
(λ̂ = -ln(1-f)/ā) fixes it (1.50 vs 1.77) but changes the specified update
rule; the full stack (d) passes regardless via the 304 cost model. Tracked
as a follow-up on #146 — needs a team decision before changing.

## 1. Problem

Today the crawler knows *what* to crawl first (OPIC score, T2) but not *what to
revisit and when*. The recrawl-due set is drawn by an unordered `LIMIT` scan
(`selectFromSnapshot`, `cmd/website-crawler/main.go:1289`) — high-value,
fast-changing pages wait behind stable ones for up to `RECRAWL_DAYS` (floor
120d). Fresh-index lag is therefore budget-agnostic: adding crawl capacity does
not preferentially reduce staleness where it matters.

HE-4 adds: change detection (what signals staleness), staleness scoring, a
recrawl priority queue, and politeness-aware scheduling across the fleet.

## 2. Existing building blocks (survey, main @ de11626)

| Piece | Where | State |
|---|---|---|
| Recrawl eligibility | `main.go:recrawlDays()`, `minRecrawlDays=120` | Global floor; page must be that old before refetch |
| T3 adaptive interval | `domain_info.recrawl_interval_days`, `adaptive.go`, `main.go:1003–1007, 1289–1294, 2133–2150` | Per-domain: 304 → ×1.25 (cap 30d); 200-changed → ×0.75 (floor 1d). **Honored only in the snapshot path** — legacy draw ignores it |
| Conditional GET | `website_info.etag` / `last_modified`, `main.go:417, 455–456, 882–891` | Sent as If-None-Match / If-Modified-Since; 304 → `crawled_at` touch only (`main.go:2116–2121`) |
| SimHash IG2 | `website_info.simhash`, `simhash_b0..b3` | Near-dup detection today; change-signal potential untapped |
| Sitemap lastmod | `cmd/website-crawler/sitemap.go:8–10`, `internal/sitemap.LatestLastMod` | Parsed, **explicitly unused** for scheduling |
| OPIC priority | `opic.go`, `main.go:1211` | Orders only the *fresh* frontier; recrawl set unordered |
| Politeness floors | `main.go:262–276`, `frontqueue.go` | ≥1s host/IP gap, ≤20 targets/group/batch, ASN circuit breaker, robots Crawl-delay ≤30s — all **per-node local** |
| Granularity | everywhere | Recrawl is **domain-level (homepage) only**; no URL-level recrawl |

## 3. Change-detection signals

Three signals, ordered by cost (cheapest first). Each recrawl emits one of them;
the scoring layer consumes all three.

1. **Conditional-GET validators (cheapest — already deployed).** `etag` /
   `last_modified` already stored and sent. A 304 is a definitive
   *unchanged* verdict: zero content cost, only the timestamp touch. Signal:
   binary (changed / unchanged).
2. **SimHash delta (cheap — new).** Compute the page SimHash on every fetch
   (already done for IG2 dedup) and compare Hamming distance to the stored
   fingerprint. Distance > threshold (e.g. ≥8 bits on 64) = content changed,
   even when the server sends no validators. This is the *gradual-change*
   signal validators miss, and the primary driver for T3 interval adaptation.
3. **Sitemap lastmod (free — parse-only, already available).** During the
   existing sitemap fetch pass (T1/IG3), persist `SitemapLastMod` per domain.
   A `lastmod` newer than our `crawled_at` is a *pre-fetch* staleness hint:
   it lets the scheduler promote a domain *before* spending a fetch, which is
   the cheapest possible prioritization signal.

Signal fusion: validators/SimHash update the *observed change rate* (§4); the
sitemap hint updates the *estimated current staleness* without a fetch.

## 4. Staleness scoring

Per-domain state (all already in `domain_info` except `change_rate`):

- `importance` — OPIC score (`opic_history + opic_cash`), already maintained.
- `change_rate λ` — **new column** `domain_info.change_rate REAL` (changes/day,
  EWMA). Updated on every recrawl: SimHash-delta or non-304 → observed change;
  304 → no change. EWMA α ≈ 0.3, clamped to [1/365, 2.0].
- `overdue` — `age_days / effective_interval_days`, where
  `effective_interval = COALESCE(recrawl_interval_days, RECRAWL_DAYS)` (T3
  interval stays the mechanism; λ informs its adaptation, see below).
- `expected_gain` — probability the page changed since our copy:
  `1 - exp(-λ · age_days)`.

**Priority score:** `P = importance × expected_gain × overdue`

- `importance` biases budget toward pages the index values (OPIC = link-earned
  importance).
- `expected_gain` biases toward pages likely to have changed (Poisson model on
  λ). A daily news homepage with λ=1.0 outranks a static brochure site with
  λ=1/365 at equal age.
- `overdue` biases toward pages past their due date, preventing starvation of
  low-λ pages (they still get crawled, just on their long interval).

**T3 interplay:** T3's multiplicative interval adaptation stays, but its
signals upgrade: "changed" is now the SimHash-delta verdict (not just "200
with content"), and the tighten/backoff factors become functions of λ rather
than constants — fast changers tighten harder (×0.5), slow changers back off
softer. Concretely: `recrawl_interval_days` converges to ≈ `1/λ` clipped to
[T3 floor, T3 cap]; the EWMA does the learning, T3 keeps the guardrails.

## 5. Recrawl priority queue design

Replace the unordered `LIMIT` stale draw in `selectFromSnapshot` with a
score-ordered draw:

```sql
SELECT domain, ip, etag, last_modified
  FROM domain_info
 WHERE web_crawled_at < NOW() - make_interval(days => COALESCE(recrawl_interval_days, $1))
   AND dns_fail_count < 3
 ORDER BY (opic_history + opic_cash)
        * (1 - exp(-COALESCE(change_rate, 1.0/120) * EXTRACT(EPOCH FROM (NOW() - web_crawled_at))/86400))
        * (EXTRACT(EPOCH FROM (NOW() - web_crawled_at))/86400 / COALESCE(recrawl_interval_days, $1))
        DESC
 LIMIT $2
```

Supporting index: expression index on the score components, or (cheaper at
20M rows) a **materialized `next_score` column** refreshed by a background
sweeper: `UPDATE domain_info SET next_score = P(...) WHERE web_crawled_at < ...`
plus a partial btree on `(next_score DESC) WHERE recrawl-due predicate`. The
sweeper runs on a cadence (e.g. every 15 min) and after each batch ingest, so
the hot draw is an index range scan, not a full sort. New columns:
`change_rate REAL`, `next_score REAL`, `sitemap_lastmod TIMESTAMPTZ` (all
NULL-able, backfilled lazily — no migration downtime).

The existing downstream machinery is untouched: the ordered stale set flows
into the same `capBatch` de-clustering, ASN/IP caps, breaker checks, and
front-queue politeness pacing. Only the *order of consideration* changes.

Conditional-GET remains the fetch path: validators are already selected with
each target, so the priority queue costs no extra requests.

## 6. Politeness-aware scheduling (single node)

Politeness floors (§2) are hard constraints, not knobs — the priority queue
never overrides them:

- Host/IP ≥1s gap, ≤20 targets/group/batch, robots Crawl-delay, circuit
  breaker: all enforced downstream of the queue as today.
- Backpressure interaction (#129 `rateGate`): when central returns 429/503
  with Retry-After, the crawl loop pauses; the queue is re-drawn on resume so
  overdue scores are recomputed (no stale-priority fetch).
- **Budget accounting:** each batch records `fetches_spent` vs the node's
  configured budget; the score's marginal-gain curve (§8 simulation) tells us
  the knee where extra budget stops buying freshness.

## 7. Fleet scheduling (PROVISIONAL — needs HE-1 #135)

HE-1 shards the website-crawl by registrable domain (consistent hashing +
vnodes); each node owns its shards' politeness budget. HE-4 plugs in as:

- **Local queues, global assignment:** each node runs §5's priority queue
  restricted to `shard(domain) ∈ my_shards` (the HE-1 draw filter). No
  cross-node priority mesh — shard ownership *is* the fleet scheduling.
- **Central due-hints via heartbeat:** the HE-1 heartbeat response gains an
  optional `recrawl_hints` field — central may inject high-priority domains
  (e.g. news spikes detected server-side) as `[{domain, boost}]`. Node merges
  boosts multiplicatively into P for one draw cycle. Opt-in, additive, and
  ignorable under load — central never *commands* a crawl, it only suggests.
- **No-double-recrawl:** HE-1's claim-before-crawl leases already prevent two
  nodes recrawling the same domain; HE-4 adds nothing here.
- **Freshness SLA per shard:** heartbeat metrics gain `p50_staleness_days`
  and `pct_overdue` per node; central aggregates to a fleet freshness
  dashboard (server side, Claude's side of the wire).

Until #135's PROTOCOL v2 is agreed, the heartbeat extensions above are spec
only — no client code.

## 8. Simulation plan (before implementation)

Go test harness (not prod infra), synthetic fleet:

- **Workload:** N virtual nodes, D synthetic domains with mixed λ (Zipf:
  1% daily-changers, 10% weekly, rest monthly/yearly), Zipf importance.
  Change events Poisson(λ); fetches cost 1 budget unit; 304 costs 0.1.
- **Policies compared:** (a) today's uniform LIMIT draw; (b) §5 priority draw;
  (c) priority + sitemap-hint pre-promotion; (d) priority + conditional-GET
  (the full §3–§5 stack).
- **Metrics:** fresh-index lag = Σ importance × age-of-stale-copy (the
  quantity P optimizes); lag-vs-budget curves per policy; % of budget spent
  on 304s; max overdue per importance decile (starvation check).
- **Success bar:** policy (d) cuts importance-weighted lag ≥50% vs (a) at
  equal budget, with no importance decile starved (max overdue bounded by
  2× its interval).

## 9. Open questions

1. **Claude (HE-1):** heartbeat `recrawl_hints` + `p50_staleness_days` /
   `pct_overdue` metrics — acceptable as PROTOCOL v2 additions? (§7)
2. **Chris:** is a materialized `next_score` column + sweeper acceptable, or
   preferred to keep the score purely computed in the draw query? (DDL-adjacent
   decision; sweeper is app-side, no schema lock beyond ADD COLUMN.)
3. SimHash delta threshold: 8 bits is a starting guess — calibrate against
   the labeled near-dup set from IG2 before locking it in.
4. Sitemap `lastmod` trust: many sitemaps stamp `lastmod=now()` on every
   build. Treat as hint (promotion), never as the sole staleness signal —
   the fetch-time SimHash delta remains ground truth.
