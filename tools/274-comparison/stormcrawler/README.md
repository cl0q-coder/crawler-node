# StormCrawler leg — #274 crawler comparison

Head-to-head vs crawler-node/Colly/Scrapy/Heritrix/Nutch under identical
conditions: 1000 fixture hosts, 1s/host politeness floor (configured), 16
fetcher bolts, robots.txt per host, 1 page per host. **StormCrawler 2.11**
(latest stable 2.x) on Storm 2.6.0, local mode.

## Run

    bash run-bench274.sh        # full 1000-seed run
    bash run-bench274.sh 20     # smoke test

`run-bench274.sh` builds the topology classes via `build.sh` (curl-based
dependency resolution with `resolve_deps.py` + `javac` — Maven Wagon's HTTP
client cannot use this sandbox's egress proxy; on a normal network
`mvn -DskipTests package` from `pom.xml` works too), generates a throwaway
CA + wildcard `*.test` cert, builds the logging fixture (shared source with
the Nutch leg: `../nutch/fixturelog`, a copy of `cl0q/tools/bench-fixture`
plus a per-request `RequestLog`), bind-mounts the bench hosts file over
/etc/hosts, and runs the topology timed via `../runstat.py`.
`../nutch/analyze_requests.py` then reports per-host robots.txt → page gaps
from the fixture's request log.

Work dir defaults to `~/workspace/stormc274` (outside the repo): JDK 17
(`jdk17/`, portable Temurin — backend boxes vary), dependency repo
(`m2repo/`), TLS material, hosts file, fixture binary, and logs all live
there. Classes are rebuilt only if `target/classes/bench274/Bench274.class`
is missing.

## Topology (`bench274.Bench274`)

`FileSpout(seeds)` → `URLPartitionerBolt` → `FetcherBolt ×16`
(`fieldsGrouping` on `key` = host partition) → `CountingStatusBolt`
(`fieldsGrouping` on the `status` stream, field `url`). The process exits 0
when all N seeds produced exactly one status tuple (600s timeout otherwise).

Config (mirrors the other legs' semantics):

| key | value | note |
|---|---|---|
| `fetcher.server.delay` | 1.0 | 1s politeness floor (default is also 1.0) |
| `fetcher.server.min.delay` | 0.0 | |
| `fetcher.threads.number` | 1 | 1 thread per fetcher bolt → 16 in flight, like the other legs |
| `fetcher.threads.per.queue` | 1 | |
| `fetcher.queue.mode` | byHost | |
| `http(s).protocol.implementation` | `...protocol.okhttp.HttpProtocol` | `http.trust.everything` defaults true → no truststore needed |
| `http.agent.name` | `benchstorm` | RFC 9309 token (letters only — 2.11 hard-fails on digits) |
| `sitemap.discovery` | false | no sitemap autodiscovery noise |

## Key architectural finding

`FetcherBolt` is "adapted from Apache Nutch" (its own javadoc) and shares
Nutch's politeness model: `fetcher.server.delay` gates consecutive **queued**
fetches per host queue (`FetchItemQueues.nextFetchTime`), but robots.txt is
fetched **inline in the fetcher thread** —
`protocol.getRobotRules(fit.url)` → `HttpRobotRulesParser` →
`http.getProtocolOutput(robotsUrl)` on cache miss (verified in the 2.11
source: `FetcherBolt.java`, `HttpRobotRulesParser.java`). With 1 page/host no
queue ever holds two items, so the configured 1s floor **never binds** —
expected robots→page gap ≈ inline-GET latency (ms), not ≥1s. Same as the
Nutch leg; different from Heritrix (robots.txt as a queued work item,
measured 1.031s min gap).

## Environment gotchas (all local-only)

1. **IPv6**: same as Heritrix/Nutch — `-Djava.net.preferIPv4Stack=true`.
2. **DNS**: StormCrawler resolves via the JVM (`InetAddress` → /etc/hosts),
   so the bind-mounted `$W/hosts.new` works (same as Nutch; unlike Heritrix's
   dnsjava).
3. **TLS**: okhttp `HttpProtocol` ships with `http.trust.everything`
   defaulting to **true** — no cert validation against the fixture, no
   truststore import. (A throwaway CA + `*.test` wildcard cert is still
   generated and served, keeping the TLS handshake itself real.)
4. **Proxies**: unset all `*_proxy` env vars — otherwise Java/curl route the
   loopback fixture through the sandbox egress proxy.
5. **/tmp is wiped between sessions on this box** — the hosts file, certs,
   fixture binary, and logs all live under `$W` (`~/workspace/stormc274`),
   never /tmp.
6. **Java**: 17 — a portable Temurin 17 lives at `~/workspace/stormc274/jdk17`
   (backend boxes vary; `/usr/lib/jvm` is not guaranteed). Storm 2.6.0
   supports 11/17. Heap capped at 1536m.
