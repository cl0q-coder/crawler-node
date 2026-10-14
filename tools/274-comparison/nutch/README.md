# Nutch leg — #274 crawler comparison

Head-to-head vs crawler-node/Colly/Scrapy/Heritrix under identical conditions:
1000 fixture hosts, 1s/host politeness floor (configured), 16 fetch threads,
robots.txt per host, 1 page per host. **Apache Nutch 1.23** (1.x runs
standalone in Hadoop local mode; 2.x would need Gora backends).

## Run

    bash run-bench274.sh

The script downloads + sha512-verifies the Nutch 1.23 bin tarball into
`$NUTCH274_HOME` (default `~/workspace/nutch274`, outside the repo),
generates a throwaway CA + wildcard `*.test` cert, builds `fixturelog/`,
bind-mounts the hosts file, and runs
inject → generate → fetch (timed) → parse → updatedb at depth 1.
`analyze_requests.py` then reports per-host robots.txt → page gaps from the
fixture's request log.

`fixturelog/` is a copy of `cl0q/tools/bench-fixture` plus a `RequestLog`
hook (one `<unixnano> <host> <path>` line per request) so per-host politeness
gaps can be measured server-side — Nutch's own logs don't timestamp the
robots fetch.

## Key architectural finding

Nutch's `fetcher.server.delay` (set to 1.0s here) gates **consecutive queued
fetches to the same host queue** (`FetchItemQueue.nextFetchTime`, set on
`finishFetchItem`). But robots.txt is fetched **inline, outside the queue**:
`FetcherThread` dequeues the page → `protocol.getRobotRules()` (synchronous
robots GET) → `protocol.getProtocolOutput()` (page GET). With 1 page/host,
no queue ever holds two items, so the configured 1s floor **never binds** —
the measured robots→page gap is ~0.05–0.2s, not ≥1s. Verified in the 1.23
source (`FetcherThread.java`, `FetchItemQueue.java`). This is a real
politeness-model difference vs Heritrix (which queues robots.txt as a work
item and measured a 1.031s min gap), not a config mistake.

## Results (2026-10-13, 2-CPU EPYC)

- runstat (fetch step): wall_s=29.41, peak_rss_tree_mb=563.7, cpu_tree_s=29.9, rc=0
  (repeat run: 29.58s / 558.1MB / 30.4s — stable)
- Nutch's own fetch-phase elapsed: 24.189s (~5.2s JVM startup inside the wall)
- fetches: 1000 robots.txt + 1000 pages, 1000/1000 success, 0 errors,
  0 robots-denied; 67.2MB downloaded (≈ expected 67.4MB for the page mix)
- pages/s (full-process wall, same basis as the other legs): **34.00**
- pages/s (fetch phase only): 41.3
- per-host robots→page gap: min=0.049s, p50=0.107s, max=41.5s (one outlier),
  **999/1000 below the 1s floor** → floor NOT honored by design (see above)
- fetch latency (Nutch counter): p50=139ms, p95=289ms — a fresh loopback TLS
  GET measures ~46ms here, so ~95ms/host is Nutch-side per-fetch overhead

## Head-to-head (1000 seeds, 16 workers, same box)

| crawler               | pages/s | min per-host gap | peak RSS | CPU   | notes                                  |
|-----------------------|--------:|-----------------:|---------:|------:|----------------------------------------|
| crawler-node (phase1) |   15.70 | 1000ms (floor)   | ~18MB heap | —   | faithful harness                       |
| colly (sticky workers)|   15.79 | 996ms            | 77MB     | 2.0s  | robots→1s→page per host, 16 sync workers |
| scrapy                |   14.99 | ≥1s dispatch     | 163MB    | 12.8s | DOWNLOAD_DELAY=1, per-hostname slots   |
| heritrix 3.18.0       |   22.14 | 1.031s           | 544MB    | 48.6s | 16 toes, minDelayMs=1000, 0 failures   |
| **nutch 1.23**        |   **34.00** | **0.049s**   | 564MB    | 29.9s | 16 threads, server.delay=1.0 (non-binding at 1 page/host) |

Nutch is the fastest of the five — but the number is not directly comparable
on politeness: it is the only leg that does not observe the 1s robots→page
floor (999/1000 gaps < 1s). At equal politeness it would sit near the
~63s naive bound like the others; its per-fetch overhead (~95ms/host over
raw TLS) is the highest measured.

## Environment gotchas (all local-only)

1. **IPv6**: same as Heritrix — the sandbox misroutes IPv4-mapped IPv6 to
   127/8. `NUTCH_OPTS="-Djava.net.preferIPv4Stack=true"`.
2. **DNS**: Nutch resolves via the JVM (`InetAddress` → /etc/hosts), so the
   bind-mounted `/tmp/hosts.new` (bench{i}.test → 127.0.0.1) works with no
   extra switches — unlike Heritrix's dnsjava, which needed
   `disableJavaDnsResolves`. The script regenerates `/tmp/hosts.new` if
   missing (it's on tmpfs and does not survive reboots).
3. **TLS**: Nutch's protocol-http ships with `http.tls.certificates.check`
   defaulting to **false** — no cert validation against the fixture, so no
   truststore import was needed. (A throwaway CA + `*.test` wildcard cert is
   still generated and served, keeping the TLS handshake itself real.)
4. **Proxies**: unset all `*_proxy` env vars — otherwise Java/curl route the
   loopback fixture through the sandbox egress proxy.
5. **Java**: 17 (`/usr/lib/jvm/java-17-openjdk-amd64`); Nutch 1.23 + Hadoop
   3.5.0 start with no `--add-opens` needed. Heap capped at 1536m (box is
   RAM-tight; GC time was negligible at ~97ms total).
6. **crawler-commons warning**: `SimpleRobotRulesParser` logs
   `'bench274' is not a valid user-agent token following RFC 9309` — cosmetic;
   the fixture's `User-agent: *` allow-all rule matches regardless.
7. **Don't `pkill -f` with a pattern that appears literally in your own
   shell command line** — it matches itself (SIGTERM'd two shells learning
   this). Kill by PID file; use the `[f]ixturelog` bracket trick only when
   the literal target string is absent from the invoking command line.
