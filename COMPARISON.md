# Crawler optimization + open-source crawler comparison (#274)

Date: 2026-10-12/13. Branch: `274-crawler-optimization-comparison` (off main @ `fa74665`).
Driver harness: `tools/274-comparison/`.

## 1. Optimization work (our side)

### Baseline profile — `docs/274-baseline.md`

crawler-node fetch pipeline (`hostThrottle → robotsFor → fetchWebsiteInfo`)
profiled with pprof on a hermetic 1000-site loopback fixture
(1000 distinct `bench{i}.test` sites, 1KB/50KB/500KB page mix, fixed
permissive robots.txt, 2ms server latency, 16 workers, 2-CPU AMD EPYC 9D25).

Top-3 bottlenecks:

| # | bottleneck | share of CPU | note |
|---|------------|--------------|------|
| 1 | HTML parsing | ~47% | DOM build 23.1% + DOM walk 23.8% (SimHash 12.3%, word count 10.1%); ~2/3 of all allocations, ~14× byte amplification |
| 2 | TLS handshakes | ~18% | 1 fresh handshake per distinct host (1.8ms CPU each; PQ ML-KEM + X25519) — inherent, connections can't be shared across hosts |
| 3 | Per-(site,IP) politeness | throughput governor | robots GET → page GET ≥1s per site/IP: **15.7 pages/s** faithful vs **229.1 pages/s** pipeline ceiling (14.6× lower) |

### Bottleneck #1 fix — `docs/274-phase2.md`

On text-heavy pages word counting (`countWords` 28.6% flat + `unicode.IsSpace`
14.3%) dominated parse CPU — not the DOM build. Shipped a byte fast path
(ASCII table lookup; non-ASCII still goes through `unicode.IsSpace`):

| benchmark | before | after | delta |
|---|---|---|---|
| `BenchmarkCountWords` (180KB text) | ~820µs/op | ~228µs/op | **3.6× faster**, 0 allocs both |
| `BenchmarkParseDOM` (480KB page, end-to-end) | ~6.04ms/op | ~4.83ms/op | **~20% faster end-to-end** |

`TestCountWordsEquivalence` proves identical semantics (17 cases incl.
NBSP/EM SPACE/CJK/emoji). A single-pass tokenizer rewrite of `parseHTMLInfo`
(prototype `cmd/website-crawler/parse_singlepass.go`, field-equivalent on 22
tricky docs) measured ~wash on CPU, −38% allocs — kept as a prototype, NOT
wired into production without review. Bottleneck #2 (TLS, inherent) and #3
(politeness floor, policy call for Chris) need no code change.

## 2. Head-to-head — open-source crawler comparison

Same conditions everywhere: 1000 fixture hosts, 1s/host politeness floor,
16 workers/threads, robots.txt per host, 1 page per host, 2-CPU EPYC box.
All drivers replicate the baseline semantics (robots-first, then page ≥1s
after robots, per-host serialization) so the comparison is apples-to-apples.

### Results

| crawler | pages/s | min per-host gap | peak RSS | CPU | notes |
|---|---|---|---|---|---|
| crawler-node (faithful) | 15.70 | 1000ms (floor) | ~18MB heap | — | baseline harness |
| Colly (sticky workers) | **15.79** | 996ms | 77MB | 2.0s | robots→1s→page per host, 16 sync workers |
| Scrapy (DOWNLOAD_DELAY=1) | **14.99** | ≥1s dispatch (slot mechanics) | 163MB | 12.8s | per-hostname slots, ROBOTSTXT_OBEY |
| Heritrix 3.18.0 | **22.14** (30.8 crawl-phase; wall incl. ~8s JVM startup) | 1.031s | 544MB | 48.6s | 16 toe threads, minDelayMs=1000, 0 failures |
| Nutch 1.23 | **34.00** (41.3 fetch-phase; wall incl. ~5s JVM startup) | 0.049s | 564MB | 29.9s | 16 threads, server.delay=1.0s set but non-binding (inline robots) |
| StormCrawler 2.11 (Storm 2.6.0) | **19.13** (24.9 crawl-phase; wall incl. ~4s topology submit) | 0.003s | 867MB | 53.1s | 16 fetcher bolts, fetcher.server.delay=1.0 set but non-binding (inline robots) |

**Key finding (updated, all 7 legs):** at the politeness-bound operating
point, the drivers that honor the 1s robots→page floor land in the 15–22
pages/s band — the floor dominates, so client efficiency differences don't
show. Heritrix's BDB frontier pipelines the politeness waits better than
the naive 1000×1s/16 bound (its toes stay fed), trading ~10× the RSS and a
much heavier runtime. Nutch (34.00 pages/s) and StormCrawler (19.13
pages/s) look faster only because their `fetcher.server.delay` gates
**queued** per-host fetches while robots.txt is fetched **inline in the
fetcher thread** — at 1 page/host no queue ever holds two items, so the
configured 1s floor never binds (999/1000 gaps < 1s for both; min 0.049s
Nutch, 0.003s StormCrawler — verified in both codebases' 1.x/2.11 sources).
At equal politeness they'd sit in the same band. StormCrawler is also the
heaviest leg measured: 867MB peak RSS for the same 1000 pages.

Harness bugs caught along the way (documented so they don't bite again):
Colly `LimitRule` matches `URL.Host` *with port* (bare `bench*.test` glob
matched nothing → 350 pages/s unthrottled); Colly's `waitChan` is per-rule
not per-domain (global serialization, ~0 pages in 7 min); a global FIFO
semaphore starves pages behind robots (298 pages/s, not comparable) —
fixed with 16 sticky sync workers. Heritrix: 3.4.0 was never released
(used 3.18.0); needed `-Djava.net.preferIPv4Stack=true`,
`fetchDns/disableJavaDnsResolves=true`, and `extractorHttp` removed from
the fetch chain (hardcoded /favicon.ico per URI, no disable flag) — all
documented in `tools/274-comparison/heritrix/README.md`.

### Reproduce

```bash
cd ~/workspace/cl0q-274/crawler-node
git checkout 274-crawler-optimization-comparison
go build -o /tmp/fixture-server ./tools/bench-fixture/cmd/fixture-server
# baseline benchmarks:
go test -run 'TestBaseline274$|TestBaseline274Faithful$' -v -timeout 30m \
  ./cmd/website-crawler/ -args -fixture-bin=/tmp/fixture-server
# colly/scrapy drivers:
go run ./tools/274-comparison/colly/main.go
cd tools/274-comparison/scrapy && scrapy crawl bench
# heritrix: see tools/274-comparison/heritrix/README.md
# nutch: see tools/274-comparison/nutch/README.md
# stormcrawler: see tools/274-comparison/stormcrawler/README.md
```

## 3. Recommendations

1. **Ship the countWords byte fast path** (already on the branch, proven
   identical semantics, ~20% faster parse on text-heavy pages). It's a safe,
   reviewable 1-function change.
2. **Pipeline-ceiling comparison as follow-up**: the current numbers compare
   polite crawl throughput (dominated by the 1s floor). A ceiling-only run
   (gates armed, non-binding) would show where parse/TLS efficiency
   differences actually appear — likely where our HTML parsing gains (above)
   and Colly's lighter RSS matter.
3. **Nutch/StormCrawler legs remain**: both are JVM drivers like Heritrix;
   they will show whether Heritrix's frontier advantage is general to the
   JVM-frontier design or Heritrix-specific.

## Open questions (for Chris)

- Optimizations found: follow-up PRs in the #274 track, or separate issues?
  (Asked on #274; defaulting to follow-up PRs referencing #274.)
