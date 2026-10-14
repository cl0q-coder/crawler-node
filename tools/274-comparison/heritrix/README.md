# Heritrix 3 leg — #274 crawler comparison (phase 5)

Head-to-head vs crawler-node/Colly/Scrapy under identical conditions:
1000 fixture hosts, 1s/host politeness floor, 16 fetch threads, robots.txt
per host, 1 page per host.

## Version substitution

Requested Heritrix 3.4.0 does NOT exist: no `3.4.0` tag and no release
asset (tags are dated snapshots like `3.4.0-20240909`; dist assets ship
only on the 3.5.0+ line). Used latest stable **3.18.0**:

    https://github.com/internetarchive/heritrix3/releases/download/3.18.0/heritrix-3.18.0-dist.tar.gz

Unzip with `tar --no-same-owner -xzf` (archive owned by uid 501; plain
tar fails on chown).

Dist has no `profile/` dir; `profile-crawler-beans.cxml` lives inside
`heritrix-engine-3.18.0.jar` — extract it:

    unzip -p <dist>/lib/heritrix-engine-3.18.0.jar \
      org/archive/crawler/restlet/profile-crawler-beans.cxml \
      > profile/org/archive/crawler/restlet/profile-crawler-beans.cxml

`gen_job.py <dist> 40659` expects the profile at `<home>/../profile/...`.

## Environment gotchas (all local-only, documented here not worked around in the repo)

1. **DNS**: Heritrix resolves `dns:` prerequisite URIs with dnsjava,
   which bypasses /etc/hosts. Set
   `fetchDns/disableJavaDnsResolves=true` — FetchDNS documents this
   switch for hosts-file resolution ("only allow InetAddress
   resolution, precisely because it may use local 'hosts' files").
   Without it, every fetch dies with
   `SSLException: Unsupported or unrecognized SSL message`.
   Alternatives that FAILED: removing fetchDns from the chain
   (PreconditionEnforcer marks http URIs -50 PREREQ_FAILED); local
   DNS on 127.0.0.1:53 (sandbox blocks UDP); iptables/nft REDIRECT
   (missing kernel modules); `ip route add local` (defeated by the
   transparent proxy). Go/Python phases resolve via the OS resolver,
   so this equalizes the resolution path.
2. **extractorHttp**: hardcodes `addOutlink(curi, "/favicon.ico", ...)`
   for EVERY fetched URI with no disable flag. Removed from the fetch
   chain: with it on, 1000 extra /favicon.ico fetches interleave per
   host (robots->page gap min 1.98s vs 1.03s clean), inflating the
   politeness cost. Header-link extraction (Location/Refresh) goes
   with it — inert for this fixture (no redirects). extractorHtml
   untouched.
3. **IPv6**: Java connects via AF_INET6 to ::ffff:127.0.0.1
   (IPv4-mapped); the sandbox network layer misroutes those, so every
   JVM TLS handshake failed while curl/python worked. Fix:
   `-Djava.net.preferIPv4Stack=true` in JAVA_OPTS (verified in-JVM
   via Groovy: TLSv1.3 handshake OK after the flag).

## Launch

    bin/heritrix -a admin:bench274pw -b 127.0.0.1 -p 8443 -r bench274

(`-r/--run-job` builds+launches the job and exits when FINISHED —
ideal for `runstat.py`. The "ERROR: JVM terminated..." line from
bin/heritrix is a false alarm: its dmesg handshake races the fast
-r exit; rc=0, job FINISHED.) Wrapped by `run-bench274.sh`
(bind-mounts /tmp/hosts.new, unsets proxies, -Xmx2g
-Djava.net.preferIPv4Stack=true).

Crawl-log analysis (per-host politeness verification):

    python3 analyze_crawl.py <dist>/jobs/bench274/latest/logs/crawl.log

## Results — clean run (robots+page only), 2026-10-13

- runstat: wall_s=45.16, peak_rss_tree_mb=544.2, cpu_tree_s=48.6, rc=0
- crawl phase: RUNNING -> FINISHED = 32.5s
- fetches: 1000 dns (status 1001) + 1000 robots.txt (200) + 1000
  pages (200); 0 failures; 0 favicon fetches
- pages/s (full-process wall, same basis as colly/scrapy): **22.14**
- pages/s (crawl phase only): 30.8
- per-host robots->page gap: min=1.031s, p50=3.36s, 0 below the 1s floor
  => 1s/host politeness floor HONORED (minDelayMs=1000, delayFactor=1.0
  on the disposition bean; 16 toe threads)

Extra data point — run with /favicon.ico (extractorHttp enabled):
wall 53.32s, 4000 fetches, 0 failures, gap min 1.983s =>
1000/53.32 = 18.75 pages/s (still above Colly's 15.79).

## Head-to-head (1000 seeds, 1s/host politeness, 16 threads)

| crawler              | pages/s | notes                                  |
|----------------------|--------:|----------------------------------------|
| crawler-node (phase1) |   15.70 | faithful harness                       |
| colly (sticky workers)|   15.79 | min per-host gap 996ms                 |
| scrapy                |   14.99 | DOWNLOAD_DELAY=1, per-hostname slots   |
| heritrix 3.18.0       |   22.14 | 544 MB peak RSS, 48.6 CPU-s            |

Heritrix pipelines politeness waits across hosts better than the two
phase-1/2 drivers (their ~63s walls sit near the naive 1000*1s/16
bound; Heritrix's BDB frontier keeps all 16 toes fed). Cost: ~10x the
RSS of the Go/Python drivers and a much heavier runtime.

Nutch / StormCrawler still to run (JVM leg, remaining).
