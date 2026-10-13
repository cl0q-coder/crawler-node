# #274 Phase 2 — bottleneck #1 (HTML parsing): results

Date: 2026-10-12 (run 2026-10-13 ~04:30 UTC). Branch: `274-crawler-optimization-comparison`.

Phase-1 baseline (docs/274-baseline.md) identified the top-3 bottlenecks:
1. HTML parsing — ~47% of CPU (DOM build 23.1% + DOM walk 23.8%; SimHash 12.3%, word count 10.1%)
2. TLS handshakes — ~18% of CPU (1 fresh handshake per distinct host; PQ ML-KEM + X25519)
3. Per-(site, IP) politeness serialization — the throughput governor (15.7 vs 229 pages/s)

This doc covers the phase-2 work on bottleneck #1.

## Finding: on text-heavy pages the DOM build is NOT the bottleneck — word counting is

A pprof of the parse path on a 480KB text-heavy page showed:

| function | flat share |
|---|---|
| `countWords` | 28.6% |
| `html.Tokenizer.readByte` | 19.0% |
| `unicode.IsSpace` | 14.3% |

`countWords` + `unicode.IsSpace` = **~43% of parse CPU**. The old `countWords`
decoded every rune (`for _, r := range s`) and ran the full Unicode
whitespace table lookup per rune. The DOM node allocation itself is cheap in
CPU terms (allocation is fast; GC pressure is the real cost at scale, and the
14x allocation amplification from phase 1 stands).

## Optimization shipped: byte fast path in countWords (main.go)

HTML text is overwhelmingly ASCII. Bytes < 0x80 now take a 256-entry table
lookup; only non-ASCII bytes pay for `utf8.DecodeRuneInString` +
`unicode.IsSpace`. Semantics are provably identical (see below).

Before/after (2-CPU AMD EPYC 9D25, Go 1.25.14, 180KB lorem text):

| benchmark | before | after | delta |
|---|---|---|---|
| `BenchmarkCountWords` (180KB) | ~820µs/op | ~228µs/op | **3.6x faster**, 0 allocs both |
| `BenchmarkParseDOM` (480KB page, end-to-end) | ~6.04ms/op | ~4.83ms/op | **~20% faster end-to-end** |

Correctness: `TestCountWordsEquivalence` compares against the old rune-loop
implementation on 17 cases (empty, NBSP, EM SPACE, ideographic space,
zero-width space, CJK, emoji, combining marks, 360KB lorem) — all pass.

## Explored but NOT shipped: single-pass tokenizer parser

`cmd/website-crawler/parse_singlepass.go` reimplements `parseHTMLInfo` as one
`html.Tokenizer` pass (no DOM build). `TestParseSinglePassDifferential`
verifies field-for-field equivalence with the DOM path on 22 tricky documents
(entities, RCDATA titles, nested h1, script/style/noscript/template/svg,
foster-parented anchors, dup attrs, malformed HTML, unicode) — all pass.

Measured on the 480KB text-heavy page: **~wash on CPU** (5.9ms vs 6.0ms; the
tokenizer's `readByte` is paid by both paths), **-38% allocations**
(1392 vs 2248 allocs/op). The CPU win is small because text scanning
dominates; the allocation win is real but only matters at GC-pressure scale.

Decision: keep the implementation + differential test on the branch as a
proven prototype, but do NOT wire it into the production fetch path yet —
swapping the parser is a correctness-sensitive change that deserves
Micah/Chris eyes (and a crawl-canary) before it replaces `html.Parse`.
The countWords fast path above captures most of the available CPU win with
far less risk.

## Bottlenecks #2 (TLS) and #3 (politeness): status

- **TLS (~18%)**: one handshake per distinct host is inherent — connections
  cannot be shared across hosts, and robots GET → page GET already reuse the
  pooled connection via the shared transport (verified: 1 handshake/site in
  the profile). The PQ ML-KEM + X25519 hybrid is Go 1.25 default; disabling
  PQ would be a security-posture change — not doing unilaterally. No code
  change; documented as measured.
- **Politeness (throughput governor)**: inherent by design (1s floors per
  site/IP). Changing the floors is a policy call for Chris. No code change;
  the phase-1 numbers stand (15.7 pages/s faithful vs 229 pages/s ceiling).

## Next (phase 2 continued)

- Head-to-head vs Heritrix / Nutch / StormCrawler / Scrapy / Colly on the
  same fixture + hardware (per agreed scope) → `COMPARISON.md`.
- Wire-in decision for `parseHTMLSinglePass` (needs Micah/Chris review).
- Re-run the full baseline benchmark with the fast path to refresh the
  headline pages/s numbers.
