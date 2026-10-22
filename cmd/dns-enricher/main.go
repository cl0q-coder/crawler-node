package main

// dns-enricher — resolves DNS for domain_info rows that have no IP yet (mostly domains website-crawler found as
// outbound links). It looks up A/AAAA/CNAME on the host and MX on the registrable apex, then updates the row and
// stamps dns_checked_at on EVERY attempt, so a dead domain is retried a few times, spaced out, not forever: after
// 3 failures (dns_fail_count) the crawlers skip it.
//
// Queries go to public resolvers directly, each lookup with its own timeout. A CNAME/MX value is only overwritten
// when the answer was conclusive (success or NXDOMAIN), never on a timeout. No HTTP requests are made.
//
// Selection uses the partial index idx_domain_info_dns_todo (see initdb/01-schema.sql). LOOP=1 runs until SIGTERM /
// SIGINT, which stops it after the batch in flight is written.
//   go run ./cmd/dns-enricher

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"codeberg.org/cl0qsearch/crawler-node/dbresil"
	"codeberg.org/cl0qsearch/crawler-node/internal/config"
	"codeberg.org/cl0qsearch/crawler-node/internal/version"
	"github.com/lib/pq"
	"golang.org/x/net/publicsuffix"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func envInt(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}

// Query public resolvers DIRECTLY (round-robin), bypassing the local systemd-resolved stub at
// 127.0.0.53 — that stub serializes concurrent queries and caps bulk throughput to ~2/s. PreferGo
// so per-lookup context deadlines are honored. Load is spread across 5 resolvers (Cloudflare/
// Google/Quad9) so no single one is hammered.
var upstreams = []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53", "1.0.0.1:53", "8.8.4.4:53"}
var resolver = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, upstreams[rand.Intn(len(upstreams))])
	},
}
var lookupTimeout = 3 * time.Second // per-LOOKUP DNS budget (each of A/AAAA, CNAME, MX gets its own)

type job struct {
	id     int64
	domain string
}
type res struct {
	id                int64
	v4, v6, cname, mx string
	cnameOK, mxOK     bool // the CNAME / MX lookup returned a DEFINITIVE answer (success or NXDOMAIN),
	// not an inconclusive timeout — only then is it safe to persist "" as "none".
}

// dnsConclusive reports whether a resolver error is a DEFINITIVE answer worth persisting. err==nil
// is conclusive (a real result, possibly empty); an NXDOMAIN / "no such host" is a conclusive
// negative; a timeout or other temporary failure is INCONCLUSIVE — persisting it as "no CNAME /
// no MX" fabricates a negative that the row would carry forever after leaving the frontier.
func dnsConclusive(err error) bool {
	if err == nil {
		return true
	}
	var de *net.DNSError
	if errors.As(err, &de) {
		return de.IsNotFound
	}
	return false
}

func resolveOne(host string) res {
	r := res{}
	// Each lookup gets its OWN timeout. Sharing one budget across A/AAAA+CNAME+MX let a slow A
	// lookup exhaust the deadline so CNAME/MX failed instantly and were written back as fake "none".
	lookup := func(fn func(context.Context) error) error {
		ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		defer cancel()
		return fn(ctx)
	}

	_ = lookup(func(ctx context.Context) error {
		ips, err := resolver.LookupIPAddr(ctx, host)
		if err == nil {
			var v4, v6 []string
			for _, ip := range ips {
				if ip.IP.To4() != nil {
					v4 = append(v4, ip.IP.String())
				} else {
					v6 = append(v6, ip.IP.String())
				}
			}
			r.v4 = strings.Join(v4, ",")
			r.v6 = strings.Join(v6, ",")
		}
		return err
	})

	cerr := lookup(func(ctx context.Context) error {
		c, err := resolver.LookupCNAME(ctx, host)
		if err == nil && c != "" && !strings.EqualFold(strings.TrimSuffix(c, "."), host) {
			r.cname = c // keep raw (incl. trailing dot), matching the domain-crawler's format
		}
		return err
	})
	r.cnameOK = dnsConclusive(cerr)

	// MX on the registrable apex (e.g. blog.example.com -> example.com); www. stripped first.
	apex := strings.TrimPrefix(host, "www.")
	if e, err := publicsuffix.EffectiveTLDPlusOne(apex); err == nil && e != "" {
		apex = e
	}
	merr := lookup(func(ctx context.Context) error {
		mxs, err := resolver.LookupMX(ctx, apex)
		if err == nil {
			var m []string
			for _, mx := range mxs {
				m = append(m, strings.TrimSuffix(mx.Host, "."))
			}
			r.mx = strings.Join(m, ",")
		}
		return err
	})
	r.mxOK = dnsConclusive(merr)
	return r
}

// validateConfig checks every knob at startup (NR2). A bad value is a FATAL
// error with a clear message — never a silent clamp or silent default.
// Call this FIRST in main.
func validateConfig() {
	// Polite default: cap the heap at 2GiB. An explicit GOMEMLIMIT env wins.
	config.SetMemoryLimitDefault(2 << 30)

	config.CheckIntRange("BATCH", config.GetInt("BATCH", 2000), 1, 10000)
	config.CheckIntRange("WORKERS", config.GetInt("WORKERS", 16), 1, 64)
	config.CheckDurationRange("DNS_TIMEOUT_MS", config.GetDurationMs("DNS_TIMEOUT_MS", 3000*time.Millisecond), 500*time.Millisecond, 30*time.Second)
	config.CheckIntRange("DNS_RECHECK_HOURS", config.GetInt("DNS_RECHECK_HOURS", 12), 1, 720)
	config.CheckIntRange("DB_MAXCONNS", config.GetInt("DB_MAXCONNS", 4), 1, 50)
	// LOOP is strict-parsed so a typo can't silently change run mode.
	if raw := os.Getenv("LOOP"); raw != "" && raw != "0" && raw != "1" {
		log.Fatalf("invalid LOOP=%q: must be 0 or 1", raw)
	}
}

func main() {
	// -version before anything else (full provenance report).
	if version.HandleVersionFlag("dns-enricher") {
		return
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	validateConfig() // NR2: refuse to start on any invalid knob, before anything else runs
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	dsn := env("DATABASE_URL", "postgres://cl0q:cl0q@localhost:5432/cl0q?sslmode=disable")
	batch := envInt("BATCH", 2000)
	workers := envInt("WORKERS", 16)
	lookupTimeout = time.Duration(envInt("DNS_TIMEOUT_MS", 3000)) * time.Millisecond
	loop := env("LOOP", "1") != "0"
	recheckH := envInt("DNS_RECHECK_HOURS", 12) // min gap between retries of a failing domain; dead after 3

	db, err := sql.Open("postgres", dbresil.WithConnectTimeout(dsn, 10))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(envInt("DB_MAXCONNS", 4))
	// dns_fail_count: spaced-retry counter; a domain unresolved 3x is marked dead so the crawler skips it.
	if _, err := db.Exec(`ALTER TABLE domain_info ADD COLUMN IF NOT EXISTS dns_fail_count INT NOT NULL DEFAULT 0`); err != nil {
		log.Printf("ensure dns_fail_count: %v", err)
	}

	log.Printf("dns-enricher start: batch=%d workers=%d recheck=%dh loop=%v", batch, workers, recheckH, loop)
	var totalDone, totalIP int
	for ctx.Err() == nil {
		// New domains first (never checked → todo index), then failing-but-not-dead domains that
		// are due for a spaced re-check (recheck index). Disjoint sets, so UNION ALL.
		rows, err := db.Query(`
			(SELECT id, domain FROM domain_info
			   WHERE coalesce(ip_addresses_ipv4,'')='' AND dns_checked_at IS NULL
			   ORDER BY id LIMIT $1)
			UNION ALL
			(SELECT id, domain FROM domain_info
			   WHERE coalesce(ip_addresses_ipv4,'')='' AND dns_checked_at IS NOT NULL
			     AND dns_fail_count < 3 AND dns_checked_at < now() - make_interval(hours => $2)
			   ORDER BY dns_checked_at LIMIT $1)
			LIMIT $1`, batch, recheckH)
		if err != nil {
			log.Printf("select: %v", err)
			dbresil.SleepCtx(ctx, 30*time.Second)
			continue
		}
		var jobs []job
		for rows.Next() {
			var j job
			if rows.Scan(&j.id, &j.domain) == nil && j.domain != "" {
				jobs = append(jobs, j)
			}
		}
		rows.Close()
		if len(jobs) == 0 {
			if !loop {
				break
			}
			log.Printf("dns-enricher: caught up (done=%d, ip-found=%d) — sleeping 60s", totalDone, totalIP)
			dbresil.SleepCtx(ctx, 60*time.Second)
			continue
		}

		jobCh := make(chan job, workers*2)
		resCh := make(chan res, workers*2)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range jobCh {
					resCh <- safeResolve(j)
				}
			}()
		}
		go func() { wg.Wait(); close(resCh) }()
		go func() {
			for _, j := range jobs {
				jobCh <- j
			}
			close(jobCh)
		}()

		// Drain results into slices (just appends → no backpressure, so the workers resolve at
		// full concurrency), then write everything back in batched multi-row UPDATEs. A single
		// UPDATE...FROM (unnest($arr)) collapses ~500 rows into one round-trip and lets Postgres
		// amortize index maintenance, instead of 2000 slow per-row updates serialized over the network.
		var ids []int64
		var v4s, v6s, cnames, mxs []string
		var cnameOKs, mxOKs []bool
		var failIDs []int64
		done, withIP := 0, 0
		for r := range resCh {
			done++
			if r.v4 != "" || r.v6 != "" { // resolved: has an A/AAAA to connect to
				ids = append(ids, r.id)
				v4s = append(v4s, r.v4)
				v6s = append(v6s, r.v6)
				cnames = append(cnames, r.cname)
				mxs = append(mxs, r.mx)
				cnameOKs = append(cnameOKs, r.cnameOK)
				mxOKs = append(mxOKs, r.mxOK)
				if r.v4 != "" {
					withIP++
				}
			} else { // could not resolve an IP → spaced retry, dead after 3
				failIDs = append(failIDs, r.id)
			}
		}
		const chunk = 500
		// Resolved → write records + clear the fail counter.
		for i := 0; i < len(ids); i += chunk {
			e := i + chunk
			if e > len(ids) {
				e = len(ids)
			}
			if _, err := db.Exec(`UPDATE domain_info d
				SET ip_addresses_ipv4=t.v4, ip_addresses_ipv6=t.v6,
				    cname      = CASE WHEN t.cok THEN t.cname ELSE d.cname END,
				    mx_records = CASE WHEN t.mok THEN t.mx    ELSE d.mx_records END,
				    dns_checked_at=now(), dns_fail_count=0
				FROM (SELECT unnest($1::bigint[]) AS id, unnest($2::text[]) AS v4,
				             unnest($3::text[]) AS v6, unnest($4::text[]) AS cname,
				             unnest($5::text[]) AS mx, unnest($6::bool[]) AS cok,
				             unnest($7::bool[]) AS mok) t
				WHERE d.id = t.id`,
				pq.Array(ids[i:e]), pq.Array(v4s[i:e]), pq.Array(v6s[i:e]),
				pq.Array(cnames[i:e]), pq.Array(mxs[i:e]),
				pq.Array(cnameOKs[i:e]), pq.Array(mxOKs[i:e])); err != nil {
				log.Printf("resolved update [%d:%d]: %v", i, e, err)
			}
		}
		// Failed → bump the counter (re-stamps dns_checked_at so the next retry is spaced). A domain
		// reaching 3 is DNS-dead; the crawler's selectTargets skips dns_fail_count >= 3.
		failed, dead := 0, 0
		for i := 0; i < len(failIDs); i += chunk {
			e := i + chunk
			if e > len(failIDs) {
				e = len(failIDs)
			}
			rows, err := db.Query(`UPDATE domain_info
				SET dns_fail_count = dns_fail_count + 1, dns_checked_at = now()
				WHERE id = ANY($1) RETURNING domain, dns_fail_count`, pq.Array(failIDs[i:e]))
			if err != nil {
				log.Printf("failed update [%d:%d]: %v", i, e, err)
				continue
			}
			for rows.Next() {
				var d string
				var fc int
				if rows.Scan(&d, &fc) == nil {
					failed++
					if fc >= 3 {
						dead++
						if dead <= 10 { // sample the newly-dead so the log doesn't flood
							log.Printf("dns-dead: %s unresolved %dx — crawler will skip it", d, fc)
						}
					}
				}
			}
			rows.Close()
		}
		totalDone += done
		totalIP += withIP
		log.Printf("dns-enricher: batch done=%d ip-found=%d dns-fail=%d dead=%d (cumulative done=%d ip=%d)",
			done, withIP, failed, dead, totalDone, totalIP)
	}
	if ctx.Err() != nil {
		log.Println("stopped: shutdown signal received")
	}
}

// safeResolve runs resolveOne for one job; a panic counts as an unresolved lookup instead of killing the process.
func safeResolve(j job) (r res) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("resolve %s: panic: %v", j.domain, p)
			r = res{id: j.id}
		}
	}()
	r = resolveOne(j.domain)
	r.id = j.id
	return r
}
