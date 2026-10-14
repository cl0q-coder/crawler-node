# #274 Phase 1 — crawler-node baseline profile

Date: 2026-10-12 (run 2026-10-13 03:45 UTC). Branch: `274-crawler-optimization-comparison` (off main @ fa74665).

## What was measured

The fetch pipeline of `cmd/website-crawler` (the crawler-node site fetcher):
`hostThrottle → robotsFor` (robots.txt fetch + parse) `→ [faithful only: ipThrottle + sitePace] → fetchWebsiteInfo`
(https GET, 2MiB body cap, charset transcode, `html.Parse`, `parseHTMLInfo` incl.
SimHash fingerprint + word count, bot-challenge detection).

The full production `crawlOne()` cannot run against a loopback fixture **by
design**: it refuses any target whose IP is not public (`netguard.PublicIP`
verdict — the security boundary; the test-only `SetAllowNonPublicForTest`
hook opens only the dial path). The driver replicates `crawlOne`'s exact gate
sequencing through the production functions. Excluded: DB layer (target
selection, flush), ASN circuit breaker, sitemap discovery.

## Harness

- `tools/bench-fixture/` — hermetic fixture server: 1000 sites
  (`bench{i}.test`), homepage sizes 1KB (60%) / 50KB (500KB mix (10%),
  deterministic HTML (title, meta, 3 absolute outlinks, lorem paragraphs),
  fixed 13-line permissive `robots.txt` (no Crawl-delay), fixed 2ms
  server-side latency per response, TLS via httptest.
- `tools/bench-fixture/cmd/fixture-server/` — standalone runner so the
  fixture lives in its own OS process; otherwise the server's TLS/HTTP work
  pollutes the crawler's pprof numbers (an in-process trial inflated
  throughput 274 vs 229 pages/s on this 2-CPU box).
- `cmd/website-crawler/baseline274_test.go` — driver: DNS stubbed
  (`bench{i}.test` → distinct `127.1.x.x`, exercising the per-IP dial and
  politeness path), `netguard` test hook, `InsecureSkipVerify` on the
  transport (cert is for 127.0.0.1; the TLS handshake itself is fully
  exercised), production politeness tunables, 16 workers (= `WORKERS`
  default), 3 iterations × 1000 sites, robots cache + gates reset per
  iteration (fresh-site semantics), every site validated HTTP 200 with
  title + words (fixture breakage can't become a fake "fast" number).

## Politeness config (production floors, documented)

`HOST_DELAY_MS`/`IP_DELAY_MS` at the 1s floors (`minHostDelay/minIPDelay`),
`MAX_CONNS`(dialSem)=48, `FETCH_TIMEOUT_MS`=20000, `THROTTLE_RETRIES`=1.
Verified separately (`TestBaseline274PolitenessFloor`): 5 sequential hits to
one site land exactly **1.000–1.001s apart** — the floor binds as designed.

## Results

| run | iter 1 | iter 2 | iter 3 | mean | p50 |
|---|---|---|---|---|---|
| pipeline-ceiling (gates armed, non-binding) | 220.9 | 233.1 | 233.1 | **229.1** | **233.1** |
| production-faithful (page ≥1s after robots GET) | 15.8 | 15.8 | 15.7 | **15.7** | **15.8** |

Units: pages/s, 16 workers, 1000 distinct (site, IP) pairs per iteration.

Memory (per 1000-site iteration, post-`runtime.GC()`): heap steady **17–19MB**;
`totalAlloc` **0.93GB/iter → ~0.93MB allocated per page** vs 66KB mean page
bytes (**~14× allocation amplification**); ~77 GCs per iteration.

Profiles (client-only): `274-cpu-final.prof`, `274-mem-final.prof` (this dir).

### pprof top-20 (flat), pipeline-ceiling run — 10.22s sampled

```
flat    flat%  function
640ms   6.26%  html.(*Tokenizer).readByte
570ms   5.58%  syscall.Syscall6
550ms   5.38%  website-crawler.countWords
470ms   4.60%  unicode.IsSpace
340ms   3.33%  runtime.memmove
330ms   3.23%  runtime.scanobject          (GC)
310ms   3.03%  x/text utf8Decoder.Transform (charset transcode)
270ms   2.64%  runtime.findObject         (GC)
230ms   2.25%  html.convertNewlines
230ms   2.25%  strings.FieldsFunc
210ms   2.05%  edwards25519 feMul         (TLS: X25519)
200ms   1.96%  mlkem.fieldReduceOnce      (TLS: post-quantum KEM)
170ms   1.66%  html.(*Tokenizer).Next     (cum 12.13%)
150ms   1.47%  fnv sum64a.Write           (SimHash)
150ms   1.47%  strings.Map
130ms   1.27%  runtime mspan.base/memclr  (alloc)
130ms   1.27%  slices.Index
120ms   1.17%  edwards25519 feSquare      (TLS)
110ms   1.08%  bigmod.addMulVVW2048      (TLS)
```

Cumulative attribution of the 10.22s:

| subsystem | cum | share |
|---|---|---|
| `fetchWebsiteInfo` (whole page fetch) | 5.76s | 56.4% |
| `html.Parse` — DOM build | 2.36s | 23.1% |
| `parseHTMLInfo` — DOM walk | 2.43s | 23.8% |
| └ `simhash.Fingerprint` | 1.26s | 12.3% |
| └ `countWords` | 1.03s | 10.1% |
| TLS client handshake (X25519 + ML-KEM, 1 per site) | 1.79s | 17.5% |
| `robotsFor` (fetch + parse 13-line robots.txt) | 0.34s | 3.3% |

Allocation profile: ~65% of all heap objects are HTML parser nodes
(`parser.addElement` 16.9%, `addText` 16.3%, `Tokenizer.Token` 31.3% cum).

## Top-3 bottlenecks (with evidence)

1. **HTML parsing — ~47% of CPU.** DOM build (`html.Parse`) 23.1% plus the
   DOM walk (`parseHTMLInfo`) 23.8%. Inside the walk: SimHash fingerprinting
   12.3% and word counting 10.1% (flat leaders: `Tokenizer.readByte` 6.3%,
   `countWords` 5.4%, `unicode.IsSpace` 4.6%). It is also the allocation
   leader (~2/3 of allocated objects, 14× bytes amplification) — the GC
   scanning cost (`scanobject`/`findObject` ~5.9% flat) rides on top of it.
   The DOM is built in full and then walked twice-ish (tokenize → parse →
   walk); the 500KB pages dominate this bucket.
2. **TLS handshakes — ~18% of CPU.** One fresh handshake per site
   (connections can't be shared across distinct hosts): 1.79s cum over 1000
   sites ≈ **1.8ms CPU per handshake**. Go 1.25 negotiates post-quantum
   ML-KEM + X25519 by default (visible as `mlkem.*`, `edwards25519.*` in the
   flat top). On loopback this is pure CPU; in production it is also wall
   time (1 RTT + crypto).
3. **Per-(site, IP) politeness serialization — the throughput governor.**
   Not CPU, but it binds first: robots.txt GET and page GET to one site/IP
   must stay ≥1s apart, so fresh-site throughput ≈ workers/1s — measured
   **15.7 pages/s at 16 workers** vs the 229 pages/s pipeline ceiling (14.6×
   lower). Any Phase-2 head-to-head must either configure politeness
   equivalently on both sides or compare pipeline-ceiling numbers; otherwise
   the comparison measures sleep configuration, not crawler efficiency.

## Methodology notes / anomalies

- Caught and fixed a harness bug mid-run: calling `ipThrottle(ip)` in the
  worker prologue *and* letting `robotsFor(domain, ip)` stamp the same IP
  gate double-counts — every fresh pair slept a spurious 1s (throughput
  read 15.6 pages/s on the "ceiling" run). Production `crawlOne` stamps the
  site gate first and lets `robotsFor` own the IP-gate stamp; the harness
  now mirrors that exactly. Lesson recorded in the test file.
- Fixture is loopback: network RTT ≈ 0.05ms + 2ms artificial latency.
  Production WAN latency shifts the profile toward I/O wait; the relative
  CPU hot spots (parse ≫ TLS ≫ robots) should hold.
- Cert verification skipped (test-only); handshake crypto measured is real.
- DNS stubbed (production: 60s `dnsAnswers` cache; first-hit cost excluded).
- DB layer, ASN breaker, sitemap discovery excluded from the measured path.
- Hardware: 2 vCPU (AMD EPYC 9D25), ~8GB RAM (~0.6GB free during runs),
  Go 1.25.14, `GOMAXPROCS=2`.

## Reproduce

```bash
export PATH=$HOME/go/bin:$PATH
cd ~/workspace/cl0q-274/crawler-node
go build -o /tmp/fixture-server ./tools/bench-fixture/cmd/fixture-server
# pipeline ceiling + profiles:
go test -run 'TestBaseline274$' -v -timeout 30m \
  -cpuprofile=/tmp/274-cpu.prof -memprofile=/tmp/274-mem.prof \
  ./cmd/website-crawler/ -args -fixture-bin=/tmp/fixture-server
# production-faithful throughput (~3.5 min):
go test -run 'TestBaseline274Faithful$' -v -timeout 30m \
  ./cmd/website-crawler/ -args -fixture-bin=/tmp/fixture-server
# politeness floor check (~5s):
go test -run 'TestBaseline274PolitenessFloor$' -v ./cmd/website-crawler/ \
  -args -fixture-bin=/tmp/fixture-server
go tool pprof -top -nodecount=20 /tmp/274-cpu.prof
```

`go build ./...`, `go vet`, and `gofmt` are clean on the branch.
