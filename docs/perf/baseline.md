# Track P — P0 baseline (crawler-node)

Date: 2026-10-14. Branch: `dutch/track-p-282` (off main @ d4c28b5, post-merge-wave).
Box: linux/amd64, go1.26.9. Builds: `make dev-build` (CGO_ENABLED=0, -s -w -trimpath).

Pure measurement — no behavior change. This table is the before/after anchor for
every later P-rung change; re-measure before claiming a win.

## Binary sizes (dist/, production ldflags)

| binary | size |
|---|---|
| website-crawler | 9.0 MB |
| cl0q-node | 7.6 MB |
| domain-crawler | 7.0 MB |
| asn-enricher | 6.9 MB |
| preflight | 6.5 MB |
| dns-enricher | 6.0 MB |

Note: `cmd/favicon-scan` exists but is NOT in the Makefile `BINARIES` list, so
it is never shipped by `make build`. Either add it or remove it (P1 item).

## Fetch-pipeline load profile (website-crawler)

Harness: the #274 bench driver (`cmd/website-crawler/baseline274_test.go`,
`TestBaseline274`), external fixture (`tools/bench-fixture/cmd/fixture-server`,
own process — client-only numbers), 3 iterations x 1000 distinct sites, 16
workers, production politeness floors (hostDelay=1s, ipDelay=1s, dialSem=48).
Re-measured 2026-10-14 to confirm the 2026-10-13 numbers still hold post-wave.

| metric | iter 1 | iter 2 | iter 3 | anchor |
|---|---|---|---|---|
| throughput (pages/s) | 247.5 | 247.1 | 197.1* | mean **230.6**, p50 247.1 |
| post-iter heap | 18 MB | 17 MB | 17 MB | steady 17–18 MB |
| totalAlloc / iter | 0.76 GB | 1.53 GB (cumulative) | 2.29 GB (cumulative) | ~0.76 MB allocated/page vs 66KB mean page bytes (**~12x amplification**) |
| GCs / iter | 77 | 117 | 156 | 77+ per 1000 sites |
| peak RSS (VmHWM, client-only) | — | — | — | **68.8 MB** |

*iter 3 dip: fixture/timing noise, within the 2026-10-13 run's 220.9–233.1 spread.

## Idle / memory notes

- Fixture-server idle RSS (1000-site fixture in memory, 60s sample): steady
  **133 MB** — server-side figure, shown for completeness; the crawler client
  idles near its Go runtime baseline (~4–6 MB) and is dominated by the fixture
  only while crawling.

## Not measured this pass (P1 follow-ups)

- DB footprint (`pg_database_size` + per-table): no local schema loaded on this
  box; take from CI staging or the prod snapshot. The #126 slow-query DDL gate
  (awaiting Chris) gates the index audit, not the footprint numbers.
- Startup latency (enroll -> first fetch): needs real enrollment credentials;
  measure from the #280 stranger-path fixture or the quarantine test node.
- domain-crawler / dns-enricher / asn-enricher throughput: their 16-worker DNS
  path was profiled for A2 (#255); numbers to pull from that PR's description.

## Proposed slim targets (for Chris to confirm)

| target | value | rationale |
|---|---|---|
| crawler-node RSS @1 vCPU | <= 256 MB | issue #282 proposal; load peak 68.8 MB + heap 18 MB gives ~4x headroom for page-size growth |
| binary size | <= 12 MB each | current max 9.0 MB; keeps Docker layers + deploy artifacts small |
| alloc amplification | <= 8x per page | currently ~12x (0.76 MB/page vs 66KB bytes); #107 and the 14x-finding from 2026-10-13 say this is the biggest easy win |
