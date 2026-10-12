package main

// domain-crawler — a breadth-first link crawler that DISCOVERS new domains and adds them to domain_info.
//
//   [domain-crawler] → domain_info → website-crawler → website_info → cl0q-node forwards it to cl0q
//
// SEEDS: -seed (comma-separated) wins → else -seeds-file (SEEDS_FILE, default ./seeds.txt; "#" comments, a bare
// host means https://) → else the built-in defaultSeeds. Each cycle also seeds -db-seeds random domains already in
// domain_info, so discovery keeps finding new links instead of re-walking the same neighbourhood. -max-pages and
// -queue-cap bound each cycle's memory.
//
// POLITENESS (what https://cl0q.com/bot promises site owners). None of it can be switched off: settings can only make
// the crawler slower or gentler than the floors in the "Politeness floors" block below.
//   * every request carries the node's User-Agent, which names its operator (package botid) — not configurable;
//   * only public internet addresses are contacted (package netguard): never loopback, private networks,
//     link-local / cloud-metadata or other reserved ranges;
//   * robots.txt is always read first (package robotstxt): path rules (longest match, * and $ wildcards); a group
//     naming cl0q-node or any other cl0q bot name beats "*"; Crawl-delay is honoured (capped at 30 s);
//   * at most -per-host-pages (≤25) pages per site per cycle, at least -host-delay-ms (≥2 s) apart — robots.txt, its
//     http fallback and redirect hops included (other sites are served while one waits);
//   * the per-host floors are GLOBAL across the -discovery-workers fetch workers (host-aware dispatcher,
//     cmd/domain-crawler/dispatcher.go): a worker only receives a URL when its host is eligible, so the same
//     host is serialized across workers while different hosts proceed in parallel. Concurrency changes the
//     execution model, NOT the floors;
//   * redirects are followed only on the same host, at most 5 hops, each robots-checked and paced; a redirect to any
//     other host is handed back to the queue, where it gets that host's own robots.txt and pacing;
//   * login, admin, account, cart, search and query-string URLs and non-HTML files are never fetched;
//   * a site that drops, resets or times out its robots.txt request is skipped for the cycle.
//
// Rows carry found_on (the page the domain was seen on). Inserts are deduplicated with WHERE NOT EXISTS
// (domain_info.domain has a non-unique index) and written DB_BATCH at a time.
//
// SHUTDOWN: SIGTERM / SIGINT stop the cycle after the page in flight; discovered domains still buffered are written
// before the process exits.
//
//   CL0Q_NODE_ID=my-node go run ./cmd/domain-crawler -seed https://example.com/ -max-depth 3

import (
	"context"
	"database/sql"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lib/pq"
	"golang.org/x/net/html"

	"cl0q/crawlutil"
	"cl0q/dbresil"
	"cl0q/internal/config"
	"cl0q/internal/footprint"
	"cl0q/internal/version"
	"cl0q/netguard"
	"cl0q/robotstxt"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

var (
	seedFlag      = flag.String("seed", os.Getenv("SEED_URL"), "comma-separated start URLs; overrides -seeds-file and the built-in defaults")
	seedsFileFlag = flag.String("seeds-file", envOr("SEEDS_FILE", "seeds.txt"), "newline-delimited seed URL file (# comments ok)")
	maxDepth      = flag.Int("max-depth", 4, "max BFS depth from the seed")
	dsnFlag       = flag.String("dsn", envOr("DATABASE_URL", "postgres://cl0q:cl0q@localhost:5432/cl0q?sslmode=disable"), "PostgreSQL DSN")
	idleFlag      = flag.Duration("idle", 60*time.Second, "pause before re-seeding when the queue drains")
	maxPages      = flag.Int("max-pages", 5000, "max pages per BFS cycle before re-seeding (bounds memory)")
	maxConns      = flag.Int("max-conns", 4, "max DB connections (dispatch loop is single-goroutine; capped for the shared 100-conn DB)")
	dbSeedsN      = flag.Int("db-seeds", 200, "random existing domain_info domains added as seeds each cycle (0=off) — mines known domains for new outbound links to beat the fixed-seed plateau")
	batchFlag     = flag.Int("db-batch", envInt("DB_BATCH", 200), "discovered domains buffered per INSERT (one commit) — fewer commits = far less remote DB load")
	noBackfillFlag = flag.Bool("no-subdomain-backfill", false, "skip the background backfill of registrable_domain/is_subdomain for pre-existing rows")
	maxSubsPerParentFlag = flag.Int("max-subdomains-per-parent", envInt("MAX_SUBDOMAINS_PER_PARENT", 10000), "max subdomains stored per registrable parent (0=off) — caps runaway discovery from wildcard DNS or huge platforms")
	queueCap      = flag.Int("queue-cap", envInt("QUEUE_CAP", 50000), "max URLs queued at once (bounds BFS memory; 0=unbounded). Fan-out pages can otherwise push this to 500k+ and OOM the box.")
	// A2 (#133): concurrent fetch workers. Tunable DOWN only — fewer workers is gentler; the politeness floors
	// stay one-directional, so more workers can never buy speed past them.
	discoveryWorkers = flag.Int("discovery-workers", envInt("DISCOVERY_WORKERS", maxDiscoveryWorkers), "concurrent discovery fetch workers (at most 16; tunable down only)")
	// A site gets at most perHostPages pages per cycle, at least hostDelayMs apart
	// (or its robots Crawl-delay, if longer). Other sites' pages are served while one waits, so pacing costs
	// little throughput.
	hostDelayMs  = flag.Int("host-delay-ms", envInt("HOST_DELAY_MS", minHostDelayMs), "min gap between requests to the same host (at least 2000)")
	perHostPages = flag.Int("per-host-pages", envInt("PER_HOST_PAGES", maxPerHostPages), "max pages fetched from one host per BFS cycle (at most 25)")
)

// Politeness floors. Settings can only make the crawler slower or gentler than these (see the header).
const (
	minHostDelayMs      = 2000             // between requests to one host
	maxPerHostPages     = 25               // pages per site per cycle
	minIdle             = 60 * time.Second // pause before a new cycle re-seeds (seeds are re-fetched every cycle)
	maxRedirects        = 5                // redirect hops followed, same host only
	maxDiscoveryWorkers = 16               // DISCOVERY_WORKERS ceiling: tunable down only, never up
)

// robotRules is one host's robots.txt verdict (see package robotstxt), plus whether the site refused the robots.txt
// request outright.
type robotRules struct {
	*robotstxt.Rules
	blocked bool // the robots request was dropped/reset/timed out → the site is refusing us: skip it this cycle
}

// allows reports whether u may be fetched (a nil verdict allows everything).
func (r *robotRules) allows(u *url.URL) bool { return r == nil || r.Rules.Allows(u) }

// parseRobots parses a robots.txt body.
func parseRobots(body string) *robotRules { return &robotRules{Rules: robotstxt.Parse(body)} }

// sameSite reports whether two hosts belong to the same registrable site (example.com and www.example.com do;
// a.co.ke and b.co.ke don't). Hosts with no registrable site (IP addresses) match only themselves.
func sameSite(a, b string) bool {
	ra, rb := crawlutil.Registrable(a), crawlutil.Registrable(b)
	if ra == "" || rb == "" {
		return strings.EqualFold(a, b)
	}
	return ra == rb
}

// fetchRobotsRules GETs https://host/robots.txt; plain http is tried only when https isn't offered, after the same
// per-host gap as any other request. A dropped, reset or timed-out request marks the host blocked for the cycle.
// Missing / 4xx robots.txt → allow everything. Redirects stay on the same site (at most maxRedirects, each paced).
func fetchRobotsRules(host string, gap time.Duration) *robotRules {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	client := *httpClient
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects || !sameSite(req.URL.Hostname(), via[0].URL.Hostname()) {
			return http.ErrUseLastResponse
		}
		time.Sleep(gap)
		return nil
	}
	for _, scheme := range []string{"https://", "http://"} {
		if scheme == "http://" {
			time.Sleep(gap) // the fallback is a second request to the same server: pace it
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+host+"/robots.txt", nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil || !crawlutil.HTTPSUnavailable(err) {
				return &robotRules{blocked: true}
			}
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, robotstxt.MaxBytes))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return &robotRules{}
		}
		return parseRobots(string(body))
	}
	return &robotRules{}
}

// skipURL: links we never follow — anything with a query string (search, filters, calendars, form endpoints),
// non-HTML files, and account/admin/checkout/search-style paths that WAFs and fail2ban watch for brute forcing.
var skipPathRE = regexp.MustCompile(`(?i)(^|/)(wp-login\.php|wp-admin|wp-json|xmlrpc\.php|login|log-in|logout|signin|sign-in|signup|sign-up|register|account|my-account|cart|checkout|basket|admin|administrator|auth|oauth|password|reset-password|search|cgi-bin|phpmyadmin)(/|$|\.)`)
var skipExtRE = regexp.MustCompile(`(?i)\.(pdf|zip|gz|tgz|tar|rar|7z|exe|dmg|msi|iso|apk|jpe?g|png|gif|webp|svg|ico|bmp|tiff?|mp3|mp4|m4a|mov|avi|webm|wav|ogg|css|js|json|xml|rss|atom|txt|csv|docx?|xlsx?|pptx?)$`)

func skipURL(u *url.URL) bool {
	return u.RawQuery != "" || skipExtRE.MatchString(u.Path) || skipPathRE.MatchString(u.Path)
}

// defaultSeeds — fallback used only when neither -seed nor a readable -seeds-file
// is given (keeps the crawler working even if seeds.txt goes missing). The real,
// editable list lives in seeds.txt; keep these two in rough sync. Diverse on
// purpose: tech-news aggregators, GitHub discovery pages, a huge "awesome" link
// list, Wikipedia, and the Curlie web directory.
var defaultSeeds = []string{
	"https://news.ycombinator.com/",
	"https://news.ycombinator.com/newest",
	"https://lobste.rs/",
	"https://dev.to/",
	"https://github.com/explore",
	"https://github.com/trending",
	"https://github.com/sindresorhus/awesome",
	"https://en.wikipedia.org/wiki/Lists_of_websites",
	"https://curlie.org/",
}

// loadSeeds resolves the seed list: explicit -seed (comma-separated) wins, else
// the seeds file (one URL per line, # comments), else the baked-in defaults.
func loadSeeds() []string {
	clean := func(parts []string) []string {
		var out []string
		seen := map[string]bool{}
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" || strings.HasPrefix(p, "#") {
				continue
			}
			if !strings.HasPrefix(p, "http://") && !strings.HasPrefix(p, "https://") {
				p = "https://" + p
			}
			if u, err := url.Parse(p); err == nil {
				u.Host = strings.ToLower(u.Host) // EXAMPLE.com and example.com are one site
				p = u.String()
			}
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
		return out
	}
	if s := strings.TrimSpace(*seedFlag); s != "" {
		if seeds := clean(strings.Split(s, ",")); len(seeds) > 0 {
			return seeds
		}
	}
	if *seedsFileFlag != "" {
		if data, err := os.ReadFile(*seedsFileFlag); err == nil {
			if seeds := clean(strings.Split(string(data), "\n")); len(seeds) > 0 {
				log.Printf("[INFO] loaded %d seeds from %s", len(seeds), *seedsFileFlag)
				return seeds
			}
		} else {
			log.Printf("[INFO] seeds file %s not usable (%v) — using built-in defaults", *seedsFileFlag, err)
		}
	}
	return defaultSeeds
}

// sampleDomains pulls up to n random existing domains from domain_info to use as
// extra per-cycle seeds — turning the millions of already-known domains into a
// rotating frontier whose homepages we re-crawl for NEW outbound domains. This is
// what breaks the fixed-seed dedup plateau. TABLESAMPLE keeps it cheap (samples a
// few blocks, no full scan / no ORDER BY random() across the whole table).
func sampleDomains(db *sql.DB, n int) []string {
	if n <= 0 {
		return nil
	}
	rows, err := db.Query(`SELECT domain FROM domain_info TABLESAMPLE SYSTEM (0.2)
	                       WHERE coalesce(domain,'') <> '' ORDER BY random() LIMIT $1`, n)
	if err != nil {
		log.Printf("[WARN] db-seed sample: %v", err)
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if rows.Scan(&d) == nil {
			if d = strings.TrimSpace(d); d != "" {
				out = append(out, "https://"+d+"/")
			}
		}
	}
	return out
}

// userAgent is set once in main from botid (names the node operator; not configurable).
var userAgent string

// httpClient refuses non-public addresses at connect time (netguard sees the resolved IP of every attempt) and never
// uses a proxy, so the check can't be bypassed through one. Each fetch sets its own redirect policy.
var httpClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second, Control: netguard.Control}).DialContext,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		ForceAttemptHTTP2:     true,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// validateConfig checks every knob at startup (NR2). A bad value is a FATAL
// error with a clear message — never a silent clamp or a silent fall back
// to the default. Call this FIRST in main, after flag.Parse.
// When adding a new knob, add its check here.
func validateConfig() {
	// Polite default: cap the heap at 2GiB so a naive configuration cannot
	// OOM the operator's box. An explicit GOMEMLIMIT env wins.
	config.SetMemoryLimitDefault(2 << 30)

	config.CheckIntRange("max-depth", *maxDepth, 0, 20)
	config.CheckDurationRange("idle", *idleFlag, minIdle, time.Hour)
	config.CheckIntRange("max-pages", *maxPages, 1, 100000)
	config.CheckIntRange("max-conns", *maxConns, 1, 50)
	config.CheckIntRange("db-seeds", *dbSeedsN, 0, 10000)
	config.CheckIntRange("db-batch", *batchFlag, 1, 5000)
	// queue-cap 0 = unbounded is allowed explicitly, but anything else must be sane.
	config.CheckIntRange("queue-cap", *queueCap, 0, 2000000)
	// Politeness floors are hard minimums: the crawler can only be made
	// slower or gentler than these, never faster.
	config.CheckDurationRange("host-delay-ms", time.Duration(*hostDelayMs)*time.Millisecond, time.Duration(minHostDelayMs)*time.Millisecond, time.Hour)
	config.CheckIntRange("per-host-pages", *perHostPages, 1, maxPerHostPages)
	// A2: worker count is tunable DOWN only — 1..16, never above the ceiling.
	config.CheckIntRange("discovery-workers", *discoveryWorkers, 1, maxDiscoveryWorkers)
	config.CheckIntRange("SPOOL_MAX_ROWS", config.GetInt("SPOOL_MAX_ROWS", 500000), 0, 2000000)
	config.CheckIntRange("NODE_MAX_DOMAINS", config.GetInt("NODE_MAX_DOMAINS", 1000000), 0, 10000000)
	config.CheckIntRange("NODE_MAX_PAGES", config.GetInt("NODE_MAX_PAGES", 1000000), 0, 10000000)
	config.CheckIntRange("NODE_MAX_DB_GB", config.GetInt("NODE_MAX_DB_GB", 20), 0, 1000)
	config.CheckNonEmpty("dsn", *dsnFlag)
}

func main() {
	// -version before anything else (full provenance report).
	if version.HandleVersionFlag("domain-crawler") {
		return
	}
	flag.Parse()
	validateConfig() // NR2: refuse to start on any invalid knob, before anything else runs
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	userAgent = config.PublicUserAgent() // truncated operator prefix, PROTOCOL.md §4
	// Politeness floors: a setting can only make the crawler gentler.
	// (validateConfig already rejected anything below the floors; the
	// max/min calls below are defense-in-depth.)
	*hostDelayMs = max(*hostDelayMs, minHostDelayMs)
	*perHostPages = min(max(*perHostPages, 1), maxPerHostPages)
	*idleFlag = max(*idleFlag, minIdle)
	if *batchFlag < 1 {
		*batchFlag = 1
	}

	db, err := sql.Open("postgres", dbresil.WithConnectTimeout(*dsnFlag, 10))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if dbresil.WaitForRemoteCtx(ctx, db) != nil { // wait out a database outage at startup instead of crash-looping
		return
	}
	// Shared DB has max_connections=100; this BFS is single-threaded, so a small
	// cap is plenty and guarantees we never contribute to "too many clients".
	db.SetMaxOpenConns(*maxConns)
	// NR3: bounded footprint — when the DB hits a cap, discovery pauses but
	// crawling/forwarding continues.
	fp := footprint.New(db)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS domain_info (
		id SERIAL PRIMARY KEY, domain TEXT, ip_addresses_ipv4 TEXT, ip_addresses_ipv6 TEXT,
		mx_records TEXT, cname TEXT, found_on TEXT, crawled_at TIMESTAMP)`); err != nil {
		log.Fatalf("ensure domain_info: %v", err)
	}
	// Subdomain inflation visibility: registrable parent + subdomain flag.
	// ALTERs are metadata-only and idempotent; indexes ship via the
	// cl0q-search migration (CONCURRENTLY, needs Chris's approval).
	ensureSubdomainColumns(db)
	// Probe the migration index ONCE and cache the result. Both the backfill
	// gate below and the per-flush cap check need idx_domain_info_registrable;
	// without it the backfill's WHERE registrable_domain IS NULL scan AND the
	// cap check's GROUP BY are full seq scans per batch on a 42.6M-row table.
	// While the index is absent (Chris-gated cl0q-search migration pending)
	// the backfill is skipped and the per-flush cap check runs fail-open.
	subdomainIdxReady := registrableDomainIndexReady(db)
	// Backfill pre-existing rows in the background (batched, resumable) —
	// but ONLY once the migration's index is in place, so we skip and retry
	// on the next restart instead of hammering the DB.
	if !*noBackfillFlag {
		if subdomainIdxReady {
			go backfillSubdomainColumns(db, 50000)
		} else {
			log.Printf("[WARN] subdomain backfill: skipping — idx_domain_info_registrable not present yet (cl0q-search migration pending); backfill will run on the next restart after the migration lands")
		}
	}
	if !subdomainIdxReady {
		log.Printf("[WARN] subdomain cap: idx_domain_info_registrable not present — per-flush cap check disabled (fail open) until the migration lands")
	}
	// key/value runtime settings (e.g. crawl_subdomains = on to follow subdomains).
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS cl0q_settings (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		log.Printf("[WARN] ensure cl0q_settings: %v", err)
	}

	host := "?"
	if u, e := url.Parse(*dsnFlag); e == nil {
		host = u.Host
	}
	fixedSeeds := loadSeeds()
	log.Printf("[INFO] domain-crawler: %d fixed seeds + %d db-seeds/cycle, max-depth=%d, max-pages=%d -> domain_info @ %s", len(fixedSeeds), *dbSeedsN, *maxDepth, *maxPages, host)
	log.Printf("[INFO] politeness: host_delay=%s per_host_pages=%d idle=%s max_redirects=%d robots=always workers=%d", time.Duration(*hostDelayMs)*time.Millisecond, *perHostPages, *idleFlag, maxRedirects, *discoveryWorkers)
	for i, s := range fixedSeeds {
		log.Printf("[INFO]   seed[%d] %s", i, s)
	}
	// Optional local outbox: when the database is down, spool discovered domains to SPOOL_DATABASE_URL instead of
	// dropping them, for a separate drain job to replay. Not used by the node build (nil = disabled when unset).
	spoolCap := envInt("SPOOL_MAX_ROWS", 500000)
	spool := dbresil.Open(os.Getenv("SPOOL_DATABASE_URL"), spoolCap)
	if spool != nil {
		log.Printf("[INFO] spool: local outbox enabled (cap %d rows)", spoolCap)
		ensureSubdomainColumns(spool.DB())
	}

	type item struct {
		url   string
		depth int
	}

	crawlSubs := false     // persists across cycles so a DB blip keeps the last-known value, not apex-only
	for ctx.Err() == nil { // re-seed until shutdown (continuous discovery)
		if dbresil.WaitForRemoteCtx(ctx, db) != nil { // ride out an outage instead of a wasted BFS cycle that drops everything
			break
		}
		// Apex-only vs include-subdomains is operator-controlled (cl0q_settings key crawl_subdomains).
		// Default apex-only: subdomain platforms (blogspot/tumblr/...) link thousands of
		// sibling subdomains per page, which previously exploded the BFS queue past 500k
		// URLs and OOM'd the box. Apex-only skips those links entirely, killing the fan-out.
		crawlSubs = crawlutil.LoadCrawlSubdomains(db, crawlSubs)
		// Fixed seeds + a fresh random sample of known domains each cycle, so we keep
		// exploring new neighbourhoods of the 2.5M-domain frontier instead of just
		// re-walking the same fixed seeds (which plateaus once they're exhausted).
		seeds := append([]string{}, fixedSeeds...)
		if extra := sampleDomains(db, *dbSeedsN); len(extra) > 0 {
			seeds = append(seeds, extra...)
			log.Printf("[INFO] cycle seeds: %d fixed + %d sampled from domain_info (crawl_subdomains=%v)", len(fixedSeeds), len(extra), crawlSubs)
		}
		// A2 (#133): the host-aware dispatcher runs this cycle's frontier. All
		// per-host politeness state (last fetch, pages this cycle, robots
		// verdict, in-flight reservation) is owned by the dispatcher's
		// goroutine and holds GLOBALLY across the worker pool — concurrency
		// changes the execution model, NOT the floors.
		dis := newDispatcher(dispatchConfig{
			workers:    *discoveryWorkers,
			hostDelay:  time.Duration(*hostDelayMs) * time.Millisecond,
			perHostCap: *perHostPages,
			batchSize:  *batchFlag,
			queueCap:   *queueCap,
			maxDepth:   *maxDepth,
			maxPages:   *maxPages,
			crawlSubs:  crawlSubs,
		}, realFetch, func(buf []pendDomain) int {
			// Buffer discovered domains and insert them in batches of *batchFlag (one commit per
			// batch instead of one per domain) — the main reduction in remote-Postgres load.
			if pause, reason := fp.PauseDiscovery(); pause {
				log.Printf("[INFO] %s — %d new domains not stored", reason, len(buf))
				return 0
			}
			// #234: cap subdomains per registrable parent before flushing.
			buf = filterSubdomainCap(db, buf, *maxSubsPerParentFlag, subdomainIdxReady)
			return flushDomains(db, spool, buf)
		})
		dis.start()
		dis.seed(seeds)
		lastBeat := time.Now()
		for !dis.drained() && ctx.Err() == nil {
			// Time-based heartbeat at the TOP of the loop so the log keeps
			// advancing (and the health check sees the crawler alive) even
			// while every worker is waiting out a host's gap.
			if time.Since(lastBeat) >= 45*time.Second {
				log.Printf("[INFO] pages=%d queue=%d domains_seen=%d new_inserted=%d robots_checked=%d",
					dis.pages, len(dis.queue), dis.discovered, dis.inserted, dis.robotsFetched)
				lastBeat = time.Now()
			}
			if !dis.pump(ctx) {
				break
			}
		}
		if dis.capped {
			log.Printf("[INFO] page cap %d reached (queue had %d) — re-seeding to free memory", *maxPages, len(dis.queue))
		}
		// Stop after the pages in flight; discovered domains still buffered
		// are written before the cycle ends — never lost, including on shutdown.
		dis.shutdown()
		if ctx.Err() != nil {
			break
		}
		log.Printf("[INFO] crawl drained: pages=%d domains_seen=%d new_inserted=%d — re-seeding in %s", dis.pages, dis.discovered, dis.inserted, *idleFlag)
		dbresil.SleepCtx(ctx, *idleFlag)
	}
	log.Printf("[INFO] stopped: shutdown signal received, buffered domains written")
}

// safeCrawl fetches pageURL and returns its <a href> links; recovers from parser panics. Redirects are followed only
// on the same host, when robots.txt allows the target and it isn't a skipped URL, each after gap; any other redirect
// target is returned as a link, so the BFS fetches it later under that host's own robots.txt and pacing.
func safeCrawl(pageURL string, rules *robotRules, gap time.Duration) (links []string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[WARN] panic crawling %s: %v", pageURL, r)
			links = nil
		}
	}()
	// robots.txt (rules, Crawl-delay, blocked sites) and per-host pacing are enforced by the BFS loop before this.
	req, err := http.NewRequest(http.MethodGet, pageURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	client := *httpClient
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects || !strings.EqualFold(r.URL.Hostname(), via[0].URL.Hostname()) ||
			!rules.allows(r.URL) || skipURL(r.URL) {
			return http.ErrUseLastResponse
		}
		time.Sleep(gap) // a hop is another request to the same host
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	// Bodies we don't parse are still read (up to 1 MiB) so the connection can be reused for this site's next page.
	drain := func() { io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)) }
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		drain()
		if loc, err := resp.Location(); err == nil {
			return []string{loc.String()}
		}
		return nil
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		drain()
		return nil
	}
	z := html.NewTokenizer(io.LimitReader(resp.Body, 5<<20))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return links
		case html.StartTagToken, html.SelfClosingTagToken:
			t := z.Token()
			if t.Data == "a" {
				for _, a := range t.Attr {
					if a.Key == "href" {
						links = append(links, a.Val)
						break
					}
				}
			}
		}
	}
}

func resolveLink(raw, base string) (domain, full string) {
	b, err := url.Parse(base)
	if err != nil {
		return "", ""
	}
	p, err := b.Parse(raw)
	if err != nil {
		return "", ""
	}
	if p.Scheme != "http" && p.Scheme != "https" {
		return "", ""
	}
	host := strings.ToLower(p.Hostname())
	if host == "" || net.ParseIP(host) != nil {
		return "", "" // an IP-literal link is never a site to discover
	}
	p.Host = strings.ToLower(p.Host) // EXAMPLE.com/x and example.com/x are one page
	return host, p.String()
}

// pendDomain is a DNS-resolved domain buffered for a batched insert.
type pendDomain struct {
	domain, v4, v6, mx, cname, foundOn string
	// registrable is the eTLD+1 parent (crawlutil.Registrable); isSubdomain is
	// true when domain is below its registrable parent. Populated at ingest so
	// headline counts can be reported as registrable domains, not raw rows.
	registrable string
	isSubdomain bool
}

// dnsTimeout bounds each lookup in resolveDomain: the BFS is single-threaded, so one slow resolver must not stall
// discovery. A lookup that times out leaves its field empty; dns-enricher fills it in later.
const dnsTimeout = 3 * time.Second

// resolveDomain best-effort DNS-resolves a domain and returns the row to insert (empty fields
// when a lookup fails — the domain is still recorded).
func resolveDomain(domain, foundOn string) pendDomain {
	lookup := func(fn func(context.Context)) {
		ctx, cancel := context.WithTimeout(context.Background(), dnsTimeout)
		defer cancel()
		fn(ctx)
	}
	var v4, v6, mxList []string
	var cname string
	lookup(func(ctx context.Context) {
		if ips, err := net.DefaultResolver.LookupIPAddr(ctx, domain); err == nil {
			for _, ip := range ips {
				if ip.IP.To4() != nil {
					v4 = append(v4, ip.IP.String())
				} else {
					v6 = append(v6, ip.IP.String())
				}
			}
		}
	})
	lookup(func(ctx context.Context) {
		if mxs, err := net.DefaultResolver.LookupMX(ctx, domain); err == nil {
			for _, mx := range mxs {
				mxList = append(mxList, mx.Host)
			}
		}
	})
	lookup(func(ctx context.Context) { cname, _ = net.DefaultResolver.LookupCNAME(ctx, domain) })
	// Subdomain tracking: registrable parent + subdomain flag, computed once at
	// ingest via the shared PSL helper (same semantics as the crawl filters).
	reg := crawlutil.Registrable(domain)
	return pendDomain{domain, strings.Join(v4, ","), strings.Join(v6, ","), strings.Join(mxList, ","), cname, foundOn, reg, crawlutil.IsSubdomain(domain)}
}

// ensureSubdomainColumns adds the subdomain-tracking columns to domain_info if
// they are missing. ALTER TABLE ... ADD COLUMN is metadata-only (no table
// rewrite) and idempotent, so it is safe to run at every startup against both
// the remote DB and the spool outbox. Indexes are NOT created here — they ship
// via the cl0q-search migration as CREATE INDEX CONCURRENTLY.
func ensureSubdomainColumns(db *sql.DB) {
	if db == nil {
		return
	}
	for _, stmt := range []string{
		`ALTER TABLE domain_info ADD COLUMN IF NOT EXISTS registrable_domain TEXT`,
		`ALTER TABLE domain_info ADD COLUMN IF NOT EXISTS is_subdomain BOOLEAN`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			log.Printf("[WARN] ensureSubdomainColumns: %v", err)
		}
	}
}

// registrableDomainIndexReady probes pg_index for the migration's
// idx_domain_info_registrable (CONCURRENTLY-built, ships via the
// Chris-gated cl0q-search migration). Fails closed: on any probe error we
// report not-ready so the backfill is skipped rather than running unindexed.
func registrableDomainIndexReady(db *sql.DB) bool {
	var ready bool
	err := db.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = 'idx_domain_info_registrable' AND i.indisvalid
	)`).Scan(&ready)
	if err != nil {
		log.Printf("[WARN] subdomain backfill index probe: %v", err)
		return false
	}
	return ready
}

// backfillSubdomainColumns fills registrable_domain / is_subdomain for rows
// that predate the columns. It runs in the background so startup is not
// blocked: each batch selects up to batchSize rows with NULL registrable_domain
// ordered by id, computes the PSL parent in Go, and updates via unnest. The
// NULL check makes it resumable — a restart picks up where it left off, and
// rows inserted concurrently by the crawler already carry values.
// On 42.6M rows expect ~15-30 minutes; progress is logged per batch.
func backfillSubdomainColumns(db *sql.DB, batchSize int) {
	log.Printf("[INFO] subdomain backfill: starting (batch %d)", batchSize)
	var total int64
	for {
		rows, err := db.Query(`SELECT id, domain FROM domain_info WHERE registrable_domain IS NULL ORDER BY id LIMIT $1`, batchSize)
		if err != nil {
			log.Printf("[WARN] subdomain backfill select: %v", err)
			return
		}
		var ids []int
		var domains []string
		for rows.Next() {
			var id int
			var domain string
			if err := rows.Scan(&id, &domain); err != nil {
				break
			}
			ids = append(ids, id)
			domains = append(domains, domain)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			log.Printf("[WARN] subdomain backfill scan: %v", err)
			return
		}
		if len(ids) == 0 {
			log.Printf("[INFO] subdomain backfill: complete (%d rows)", total)
			return
		}
		rds := make([]string, len(ids))
		sds := make([]bool, len(ids))
		for i, d := range domains {
			rds[i] = crawlutil.Registrable(d)
			sds[i] = crawlutil.IsSubdomain(d)
		}
		res, err := db.Exec(`UPDATE domain_info AS di SET registrable_domain = t.rd, is_subdomain = t.sd
			FROM unnest($1::int[], $2::text[], $3::boolean[]) AS t(id, rd, sd)
			WHERE di.id = t.id`, pq.Array(ids), pq.Array(rds), pq.Array(sds))
		if err != nil {
			log.Printf("[WARN] subdomain backfill update: %v", err)
			return
		}
		n, _ := res.RowsAffected()
		total += n
		log.Printf("[INFO] subdomain backfill: %d rows total", total)
	}
}

// filterSubdomainCap drops batch entries whose registrable parent already has
// >= maxSubs subdomains in domain_info. This caps runaway subdomain discovery
// from wildcard DNS or huge platforms (blogspot.com, tumblr.com) so they stop
// inflating the DB once the cap is reached. Non-subdomains always pass.
// Best-effort: on DB error the batch passes through unfiltered (fail open —
// discovery continues). maxSubs <= 0 disables the cap.
// idxReady is the cached startup probe of idx_domain_info_registrable: the
// GROUP BY below needs that index, and without it every flush would seq-scan
// the 42.6M-row domain_info table, so the check is skipped (fail open) while
// the Chris-gated migration is pending (logged once at startup).
// Note: counts come from the registrable_domain column, so the cap is exact
// once the startup backfill completes; until then it is approximate (fail open).
func filterSubdomainCap(db *sql.DB, batch []pendDomain, maxSubs int, idxReady bool) []pendDomain {
	if maxSubs <= 0 || len(batch) == 0 {
		return batch
	}
	if !idxReady {
		return batch
	}
	if maxSubs <= 0 || len(batch) == 0 {
		return batch
	}
	parents := map[string]bool{}
	for _, p := range batch {
		if p.isSubdomain && p.registrable != "" {
			parents[p.registrable] = true
		}
	}
	if len(parents) == 0 {
		return batch
	}
	plist := make([]string, 0, len(parents))
	for p := range parents {
		plist = append(plist, p)
	}
	counts := map[string]int64{}
	rows, err := db.Query(`SELECT registrable_domain, COUNT(*) FROM domain_info WHERE registrable_domain = ANY($1) GROUP BY registrable_domain`, pq.Array(plist))
	if err != nil {
		log.Printf("[WARN] subdomain cap check failed (fail open): %v", err)
		return batch
	}
	for rows.Next() {
		var parent string
		var n int64
		if err := rows.Scan(&parent, &n); err != nil {
			break
		}
		counts[parent] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("[WARN] subdomain cap scan failed (fail open): %v", err)
		return batch
	}
	droppedByParent := map[string]int{}
	kept := batch[:0]
	for _, p := range batch {
		if p.isSubdomain && p.registrable != "" && counts[p.registrable] >= int64(maxSubs) {
			droppedByParent[p.registrable]++
			continue
		}
		kept = append(kept, p)
	}
	for i := len(kept); i < len(batch); i++ {
		batch[i] = pendDomain{}
	}
	for parent, n := range droppedByParent {
		log.Printf("[INFO] subdomain cap: dropped %d subdomain(s) under %s (cap %d, stored %d)", n, parent, maxSubs, counts[parent])
	}
	return kept
}

// flushDomains inserts a buffer of resolved domains in ONE statement (an 8-array unnest with the
// same NOT EXISTS dedup) instead of one autocommit INSERT per domain. domain_info.domain is not
// unique, so NOT EXISTS is the dedup; the per-cycle seenDomain map already prevents in-batch dups.
// registrable_domain / is_subdomain are populated at ingest (subdomain inflation visibility).
// Returns how many NEW rows were inserted.
const diInsertSQL = `
	INSERT INTO domain_info (domain, ip_addresses_ipv4, ip_addresses_ipv6, mx_records, cname, found_on, crawled_at, registrable_domain, is_subdomain)
	SELECT t.d, t.v4, t.v6, t.mx, t.cn, t.fo, now(), t.rd, t.sd
	FROM unnest($1::text[],$2::text[],$3::text[],$4::text[],$5::text[],$6::text[],$7::text[],$8::boolean[]) AS t(d,v4,v6,mx,cn,fo,rd,sd)
	WHERE NOT EXISTS (SELECT 1 FROM domain_info di WHERE di.domain = t.d)`

func flushDomains(remote *sql.DB, spool *dbresil.Spooler, batch []pendDomain) int {
	if len(batch) == 0 {
		return 0
	}
	d := make([]string, len(batch))
	v4 := make([]string, len(batch))
	v6 := make([]string, len(batch))
	mx := make([]string, len(batch))
	cn := make([]string, len(batch))
	fo := make([]string, len(batch))
	rd := make([]string, len(batch))
	sd := make([]bool, len(batch))
	for i, p := range batch {
		d[i], v4[i], v6[i], mx[i], cn[i], fo[i] = p.domain, p.v4, p.v6, p.mx, p.cname, p.foundOn
		rd[i], sd[i] = p.registrable, p.isSubdomain
	}
	args := []interface{}{pq.Array(d), pq.Array(v4), pq.Array(v6), pq.Array(mx), pq.Array(cn), pq.Array(fo), pq.Array(rd), pq.Array(sd)}
	res, err := remote.Exec(diInsertSQL, args...)
	if err != nil {
		// remote down? spool to the local outbox instead of dropping the discovered domains.
		if spool != nil && dbresil.IsDown(remote, err) {
			if spool.OverCap("domain_info") {
				log.Printf("[WARN] spool over capacity — dropping %d discovered domain(s)", len(batch))
				return 0
			}
			sres, serr := spool.DB().Exec(diInsertSQL, args...)
			if serr != nil {
				log.Printf("[ERROR] spool insert: %v", serr)
				return 0
			}
			n, _ := sres.RowsAffected()
			log.Printf("[INFO] remote down — spooled %d discovered domain(s) to local outbox", len(batch))
			return int(n)
		}
		log.Printf("[ERROR] batch insert %d domain(s): %v", len(batch), err)
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}
