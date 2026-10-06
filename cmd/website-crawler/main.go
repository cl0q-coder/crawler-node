package main

// website-crawler — fetches the homepage of each domain in domain_info and stores what it finds in website_info.
//
//   domain-crawler → domain_info → [website-crawler] → website_info → cl0q-node forwards it to cl0q
//
// SHAPE: selectTargets picks work → capBatch de-clusters it (at most ASN_MAX_PER_BATCH per ASN / IP / site) → a
// pool of WORKERS fetches pages → ONE consumer goroutine writes the results in batches (DB_BATCH rows per
// transaction), which is why a small DB pool (DB_MAXCONNS) is enough. capBatch also de-duplicates domains:
// domain_info may hold the same domain twice, and a repeat inside one batch would break the multi-row upsert.
//
// TARGETS (USE_CRAWL_STATE=1, as docker-compose.yml sets it): domain_info.web_crawled_at is stamped on every
// attempt. Never-crawled domains come from an in-memory snapshot of the frontier (re-read every
// FRONTIER_REFRESH_S; FRONTIER_SNAPSHOT=0 uses a TABLESAMPLE per batch instead), and pages older than
// RECRAWL_DAYS are revisited. Domains whose DNS failed 3 times (dns-enricher) are skipped.
//
// POLITENESS (what https://cl0q.com/bot promises site owners). None of it can be switched off: settings can only make
// the crawler slower or gentler than the floors in the "Politeness floors" block below.
//   * every request carries the node's User-Agent, which names its operator (package botid) — not configurable;
//   * only public internet addresses are contacted (package netguard): never loopback, private networks,
//     link-local / cloud-metadata (169.254.169.254) or other reserved ranges;
//   * robots.txt is always read first (package robotstxt); a group naming cl0q-node or any other cl0q bot name beats
//     "*", and Crawl-delay is honoured (capped at 30 s);
//   * requests to one server IP and one site are at least 1 s apart — robots.txt, http fallbacks, redirect hops and
//     retries included (a target with no known IP is resolved first so it can be paced too);
//   * redirects are followed only within the same site, at most 5 hops, each checked against that host's robots.txt;
//     an off-site redirect is not followed — the new site is queued and crawled on its own terms;
//   * http:// is tried only when https:// isn't offered at all, never after a dropped or reset connection, and a
//     failed request is not retried in the same cycle;
//   * repeated timeouts, drops or throttling from one ASN / IP group open a circuit breaker that pauses the whole
//     group (1 min, doubling up to 1 h); a 429/503 is retried once only if its Retry-After is 15 s or less —
//     otherwise the site is left alone until a later cycle;
//   * bot-challenge pages (Cloudflare, Imperva, DataDome…) are detected and their content discarded, so nothing from
//     them can be indexed (cl0q-node doesn't send them at all).
//
// WRITES: one transaction per batch — a multi-row upsert into website_info, a delete of blacklisted domains
// (cl0q_blacklist), a crawled_at touch for 304 Not Modified (conditional GET with the stored ETag /
// Last-Modified), the web_crawled_at stamp, and (EMIT_DOMAINS=1) newly seen external domains into domain_info.
// NUL bytes are stripped from every field (Postgres rejects them in text).
//
// SHUTDOWN: SIGTERM / SIGINT stop handing out new work; fetches already in flight finish and their results are written
// before the process exits (docker-compose.yml allows 60 s). Targets never handed out stay unstamped for the next run.
//
// MODES: LOOP=1 (continuous) | TARGET=<domain> (one site's homepage; a bare domain, no path) | SAMPLE=<n> (n random
// known sites).
//   CL0Q_NODE_ID=my-node TARGET=example.com go run ./cmd/website-crawler

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/lib/pq"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
	"golang.org/x/net/publicsuffix"

	"cl0q/crawlutil"
	"cl0q/dbresil"
	"cl0q/internal/config"
	"cl0q/internal/footprint"
	"cl0q/internal/metrics"
	"cl0q/internal/simhash"
	"cl0q/internal/sitemap"
	"cl0q/netguard"
	"cl0q/robotstxt"
)

// -------------------- Per-host politeness --------------------
// To avoid hammering a single host (and getting rate-blocked) when a batch contains many
// names on the same site/infra — common because the subdomain enumerator inserts many
// hosts of one apex together — we gate requests per registrable domain (eTLD+1): same-host
// requests are spaced at least hostDelay apart. Gates idle for longer than gateIdle are pruned so the maps stay
// small; they are never reset wholesale, which would let the first request of a batch follow the last request of
// the previous one to the same server too soon.
type hostGate struct {
	mu   sync.Mutex
	last time.Time
	dead bool // pruned from its map: a worker still holding this gate must look the key up again
}

// gateIdle is how long a gate must be unused before it is pruned: longer than any gap it enforces (the 30 s
// Crawl-delay cap and the breaker-free pacing floors), so pruning never shortens a wait.
const gateIdle = 2 * time.Minute

var (
	hostGates sync.Map // eTLD+1 -> *hostGate
	hostDelay time.Duration
	ipGates   sync.Map // resolved IPv4 -> *hostGate (paces shared-hosting servers, not just names)
	ipDelay   time.Duration
)

func hostKey(domain string) string {
	if e, err := publicsuffix.EffectiveTLDPlusOne(strings.TrimPrefix(domain, "www.")); err == nil && e != "" {
		return e
	}
	return domain
}

// crawlSubdomains mirrors the operator's cl0q_settings toggle (crawlutil.LoadCrawlSubdomains); reloaded each batch.
// Default false (apex-only) so we don't emit/discover subdomains and feed the fan-out runaway.
var crawlSubdomains atomic.Bool

// throttle blocks until at least delay has elapsed since the last request recorded on
// `key` in `gates`, then stamps "now". Uncontended keys pass through with no wait. `gates`
// is passed by pointer so callers always read the current map even after a batch reset
// reassigns the package var.
func throttle(gates *sync.Map, key string, delay time.Duration) {
	if delay <= 0 || key == "" {
		return
	}
	for {
		// Fast path: the key almost always exists already. A plain Load avoids
		// the &hostGate{} allocation that LoadOrStore performs on every call,
		// even when the key is present and the fresh value is discarded.
		gi, ok := gates.Load(key)
		if !ok {
			gi, _ = gates.LoadOrStore(key, &hostGate{})
		}
		g := gi.(*hostGate)
		g.mu.Lock()
		if g.dead {
			// The gate was marked dead but is still in the map (prune's delete
			// raced us or hasn't run yet). Swap in a fresh gate instead of
			// spinning on the dead one forever. Zero last = no wait: a new
			// gate has no prior request to pace against.
			g.mu.Unlock()
			fresh := &hostGate{}
			if !gates.CompareAndSwap(key, g, fresh) {
				continue // someone else replaced it; reload
			}
			g = fresh
			g.mu.Lock()
		}
		if wait := delay - time.Since(g.last); wait > 0 {
			time.Sleep(wait)
		}
		g.last = time.Now()
		g.mu.Unlock()
		return
	}
}

// pruneGates drops gates unused for longer than idle. A gate in use (locked) is skipped; a pruned gate is marked dead
// under its lock, so a worker that loaded it just before the delete looks the key up again instead of pacing on an
// orphan.
func pruneGates(gates *sync.Map, idle time.Duration) {
	gates.Range(func(k, v any) bool {
		g := v.(*hostGate)
		if g.mu.TryLock() {
			if time.Since(g.last) > idle {
				g.dead = true
				gates.CompareAndDelete(k, g)
			}
			g.mu.Unlock()
		}
		return true
	})
}

// hostThrottle spaces requests to the same registrable host (eTLD+1) by at least hostDelay.
func hostThrottle(domain string) { throttle(&hostGates, hostKey(domain), hostDelay) }

// ipThrottle spaces requests to the same server IP by at least ipDelay — so a batch full of
// distinct names that all resolve to one shared-hosting box doesn't hammer that one server
// (which is what earns "too many requests / bad bot" reports even when each host is hit once).
func ipThrottle(ip string) { throttle(&ipGates, ip, ipDelay) }

// T1: sitemap discovery state. sitemapChecked records the last time we fetched
// sitemaps for a domain; rechecks are throttled to sitemapRecheckInterval so a
// sitemap isn't refetched on every batch cycle (robotsCache is cleared per batch,
// but sitemaps are bigger and change slower than robots.txt).
var sitemapChecked sync.Map // domain -> unix seconds of last sitemap check

const sitemapRecheckInterval = 24 * time.Hour

// pruneSitemapChecked drops sitemapChecked entries older than
// sitemapRecheckInterval, bounding the map to one entry per domain checked in
// the last 24h. Without this the map grows one entry per crawled domain,
// forever (the same leak class as the robotsCache leak of 2026-07-13).
func pruneSitemapChecked() {
	cutoff := time.Now().Add(-sitemapRecheckInterval).Unix()
	sitemapChecked.Range(func(k, v any) bool {
		if v.(int64) < cutoff {
			sitemapChecked.Delete(k)
		}
		return true
	})
}

// userAgent comes from botid: the good-bot form "Mozilla/5.0 (compatible; cl0q-node/1.0; operator=<id>; +<url>)".
// It never forges a real browser build — that trips WAF "client inconsistency" rules and reads as a deceptive bot.
var fpChecker *footprint.Checker // NR3: nil until main() opens the DB

// NR6: observability. Initialized in main(); nil in tests that don't call it.
var mreg *metrics.Registry
var mCrawledOK, mCrawledFailed, mCrawledSkipped, mCrawledChallenged, mCrawledUnchanged, mCrawledDeferred, mCrawledBlocked, mCrawledDuplicates, mSitemapFound *atomic.Int64
var sitemapFetch bool
var mBreakerOpen *atomic.Int64

var (
	dialSem         chan struct{}
	userAgent       string
	fetchTimeout    time.Duration
	throttleRetries int           // retries for a 429/503 whose Retry-After we can wait out (0 or 1)
	throttleWaitMax time.Duration // longest Retry-After we wait for; a longer one ends the attempt until a later cycle
	perGroupCap     int           // targets per ASN / IP / site in one batch
	robotsCache     sync.Map      // host -> robotsInfo (rules + Crawl-delay)
)

// Politeness floors. Settings can only make the crawler slower or gentler than these, never faster or less careful:
// a node crawls under cl0q's name, and https://cl0q.com/bot promises site owners these limits.
const (
	minHostDelay   = time.Second      // between requests to one site (eTLD+1)
	minIPDelay     = time.Second      // between requests to one server IP
	maxPerGroup    = 20               // targets per ASN / IP / site in one batch
	minRecrawlDays = 120              // a homepage is revisited at most this often
	maxRedirects   = 5                // redirect hops followed (same site only)
	maxRetryWait   = 15 * time.Second // longest Retry-After we wait for before the single retry
	breakThreshold = 5                // failures within breakWindow that open a group's circuit breaker
	breakWindow    = time.Minute
	breakBase      = time.Minute // first cooldown; doubles with each consecutive trip
	breakCap       = time.Hour   // cooldown ceiling
)

// clampInt pulls a setting back into [lo, hi] (zero and negative values included).
func clampInt(v, lo, hi int) int { return min(max(v, lo), hi) }

// envDuration reads a millisecond setting.
func envDuration(k string, def time.Duration) time.Duration {
	return time.Duration(envInt(k, int(def/time.Millisecond))) * time.Millisecond
}

// loadPoliteness reads the politeness settings, clamped to the floors above. robots.txt, the circuit breaker and
// the no-retry-after-a-failure rule have no setting at all.
func loadPoliteness() {
	hostDelay = max(envDuration("HOST_DELAY_MS", minHostDelay), minHostDelay)
	ipDelay = max(envDuration("IP_DELAY_MS", minIPDelay), minIPDelay)
	perGroupCap = clampInt(envInt("ASN_MAX_PER_BATCH", maxPerGroup), 1, maxPerGroup)
	throttleRetries = clampInt(envInt("THROTTLE_RETRIES", 1), 0, 1)
	throttleWaitMax = min(max(envDuration("THROTTLE_WAIT_MAX_MS", maxRetryWait), 0), maxRetryWait)
	breaker.enabled = true
	breaker.threshold = clampInt(envInt("ASN_BREAK_THRESHOLD", breakThreshold), 1, breakThreshold)
	breaker.window = max(envDuration("ASN_BREAK_WINDOW_MS", breakWindow), breakWindow)
	breaker.base = max(envDuration("ASN_BACKOFF_BASE_MS", breakBase), breakBase)
	breaker.cap = max(envDuration("ASN_BACKOFF_CAP_MS", breakCap), breakCap, breaker.base)
}

// recrawlDays is how old a page must be before it is fetched again (never less than minRecrawlDays).
func recrawlDays() int { return max(envInt("RECRAWL_DAYS", minRecrawlDays), minRecrawlDays) }

// validateConfig checks every knob at startup (NR2). A bad value is a FATAL
// error with a clear message — never a silent clamp or a silent fall back
// to the default. Call this FIRST in main, before anything reads config.
// The clampInt/max calls below remain as defense-in-depth, but after this
// function they can only see values that already passed validation.
//
// When adding a new knob, add its check here.
func validateConfig() {
	// Polite default: cap the heap at 2GiB so a naive configuration cannot
	// OOM the operator's box. An explicit GOMEMLIMIT env wins.
	config.SetMemoryLimitDefault(2 << 30)

	// Throughput / concurrency.
	config.CheckIntRange("WORKERS", config.GetInt("WORKERS", 16), 1, 64)
	config.CheckIntRange("MAX_CONNS", config.GetInt("MAX_CONNS", 48), 1, 256)
	config.CheckIntRange("FETCH_TIMEOUT_MS", config.GetInt("FETCH_TIMEOUT_MS", 20000), 2000, 60000)
	config.CheckIntRange("DB_MAXCONNS", config.GetInt("DB_MAXCONNS", 10), 1, 50)
	config.CheckIntRange("DB_BATCH", config.GetInt("DB_BATCH", 200), 1, 2000)
	config.CheckIntRange("SPOOL_MAX_ROWS", config.GetInt("SPOOL_MAX_ROWS", 500000), 0, 2000000)
	config.CheckIntRange("NODE_MAX_DOMAINS", config.GetInt("NODE_MAX_DOMAINS", 1000000), 0, 10000000)
	config.CheckIntRange("NODE_MAX_PAGES", config.GetInt("NODE_MAX_PAGES", 1000000), 0, 10000000)
	config.CheckIntRange("NODE_MAX_DB_GB", config.GetInt("NODE_MAX_DB_GB", 20), 0, 1000)
	config.CheckIntRange("BATCH", config.GetInt("BATCH", 500), 1, 5000)
	config.CheckIntRange("ASN_POOL_FACTOR", config.GetInt("ASN_POOL_FACTOR", 3), 1, 10)
	config.CheckIntRange("SAMPLE", config.GetInt("SAMPLE", 0), 0, 100000)
	config.CheckIntRange("FRONTIER_MAX", config.GetInt("FRONTIER_MAX", 400000), 1, 2000000)
	config.CheckIntRange("FRONTIER_REFRESH_S", config.GetInt("FRONTIER_REFRESH_S", 300), 60, 86400)

	// Politeness: the floors are hard minimums from https://cl0q.com/bot.
	// A setting that would make the crawler faster than the floor is
	// rejected — it can only make the crawler slower or gentler.
	config.CheckDurationRange("HOST_DELAY_MS", config.GetDurationMs("HOST_DELAY_MS", minHostDelay), minHostDelay, time.Hour)
	config.CheckDurationRange("IP_DELAY_MS", config.GetDurationMs("IP_DELAY_MS", minIPDelay), minIPDelay, time.Hour)
	config.CheckIntRange("ASN_MAX_PER_BATCH", config.GetInt("ASN_MAX_PER_BATCH", maxPerGroup), 1, maxPerGroup)
	config.CheckIntRange("THROTTLE_RETRIES", config.GetInt("THROTTLE_RETRIES", 1), 0, 1)
	config.CheckDurationRange("THROTTLE_WAIT_MAX_MS", config.GetDurationMs("THROTTLE_WAIT_MAX_MS", maxRetryWait), 0, maxRetryWait)
	config.CheckIntRange("ASN_BREAK_THRESHOLD", config.GetInt("ASN_BREAK_THRESHOLD", breakThreshold), 1, breakThreshold)
	config.CheckDurationRange("ASN_BREAK_WINDOW_MS", config.GetDurationMs("ASN_BREAK_WINDOW_MS", breakWindow), breakWindow, time.Hour)
	config.CheckDurationRange("ASN_BACKOFF_BASE_MS", config.GetDurationMs("ASN_BACKOFF_BASE_MS", breakBase), breakBase, time.Hour)
	backoffBase := config.GetDurationMs("ASN_BACKOFF_BASE_MS", breakBase)
	config.CheckDurationRange("ASN_BACKOFF_CAP_MS", config.GetDurationMs("ASN_BACKOFF_CAP_MS", breakCap), backoffBase, 24*time.Hour)
	config.CheckIntRange("RECRAWL_DAYS", config.GetInt("RECRAWL_DAYS", minRecrawlDays), minRecrawlDays, 3650)

	// Booleans: strict parse — garbage fatals instead of silently becoming false.
	config.GetBool("CRAWLER_TLS_INSECURE", false)
	config.GetBool("EMIT_DOMAINS", true)
	config.GetBool("LOOP", false)
	config.GetBool("PIPELINE", false)
	config.GetBool("USE_CRAWL_STATE", false)
	config.GetBool("FRONTIER_SNAPSHOT", true)
	config.GetBool("VERBOSE", false)

	// FRONTIER_SAMPLE_PCT: float in (0, 100].
	if raw := strings.TrimSpace(os.Getenv("FRONTIER_SAMPLE_PCT")); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v <= 0 || v > 100 {
			log.Fatalf("invalid FRONTIER_SAMPLE_PCT=%q: must be a number in (0, 100]", raw)
		}
	}
}

// DNS over TCP to public resolvers (avoids system-resolver saturation).
var resolver = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		d := net.Dialer{Timeout: 4 * time.Second}
		if c, err := d.DialContext(ctx, "tcp", "1.1.1.1:53"); err == nil {
			return c, nil
		}
		return (&net.Dialer{Timeout: 4 * time.Second}).DialContext(ctx, "tcp", "8.8.8.8:53")
	},
}

// ipDialer dials already-resolved IP literals directly: with an IP address
// there is nothing to resolve, so no Resolver is attached. DNS answers come
// from dnsAnswers (cached); netguard.Control still guards every attempt, and
// TLS SNI/verification plus the Host header keep using the URL host.
var ipDialer = &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second, Control: netguard.Control}

var transport = &http.Transport{
	DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		select {
		case dialSem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		defer func() { <-dialSem }()
		return dialCached(ctx, network, addr)
	},
	TLSClientConfig:       &tls.Config{InsecureSkipVerify: false}, // verify certs; opt out via CRAWLER_TLS_INSECURE=1 (see main)
	MaxIdleConns:          512,
	MaxIdleConnsPerHost:   4,
	IdleConnTimeout:       30 * time.Second,
	TLSHandshakeTimeout:   8 * time.Second,
	ResponseHeaderTimeout: 10 * time.Second, // free workers when servers stall on headers
	ExpectContinueTimeout: 1 * time.Second,
	ForceAttemptHTTP2:     true,
}

type WebsiteInfo struct {
	Title, Description, Keywords, Canonical, H1    string
	StatusCode                                     int
	ContentType                                    string
	ContentLength, LatencyMs                       int64
	RedirectCount, OutboundLinkCount, ImageCount   int
	WordCount                                      int
	Charset, Robots, OGTitle, OGImage, TwitterCard string
	PageLanguage, ServerHeader                     string
	ETag, LastModified                             string          // HTTP validators captured on a 200; stored so the next recrawl can send a conditional GET
	SimHash                                        uint64          // IG2: SimHash fingerprint for near-duplicate detection (0 = not computed)
	DuplicateOf                                    string          // IG2: domain of the near-duplicate original ("" if unique)
	SitemapURLs                                    int             // IG3: URLs found in sitemap.xml (0 = none/not fetched)
	SitemapLastMod                                 time.Time       // IG3: latest lastmod from sitemap (zero = none)
	outDomains                                     map[string]bool // external domains seen on the page (for discovery emit; not persisted as a column)
	Challenge                                      string          // anti-bot vendor if this was an interstitial, not the real page (not a column → routed to fetch_error)
	NotModified                                    bool            // server answered 304 to our conditional GET → content unchanged (not a column → routed to a crawled_at-only touch)
	RetryAfter                                     time.Duration   // parsed from a 429/503 Retry-After header (not a column → used to pace the retry)
}

type result struct {
	domain   string
	info     *WebsiteInfo
	err      error
	note     string // e.g. "robots_disallow"
	deferred bool   // breaker open for this target's ASN/IP → not fetched, left uncrawled for a later cycle
}

// -------------------- ASN/IP backoff (circuit breaker) --------------------
//
// The per-eTLD+1 hostThrottle does NOT protect against many hostnames sharing one
// IP/ASN (the subdomain fan-out: e.g. thousands of *.blogspot.com hosts on a handful
// of Google IPs). When such infra starts timing out (rate-blocking us), the crawler
// otherwise keeps hammering it — burning worker-seconds on the fetch timeout and
// dragging down inserts. The breaker watches per-group failures and, once a group
// trips the threshold within a window, "opens" it for an exponential cooldown during
// which its targets are deferred (skipped, left uncrawled → retried a later cycle),
// not marked failed. Group key granularity: ASN org (rollup) → IP → registrable
// parent, so it catches both enriched hosts and un-enriched subdomain fan-out.

// target carries a domain plus the infra signals used to group it for backoff/capping.
// ip/org come from domain_info/asninfo at selection time; both may be "" (un-enriched).
type target struct {
	domain string
	ip     string
	org    string
	// validators from this domain's last successful crawl → sent as If-None-Match /
	// If-Modified-Since so an unchanged page returns 304 and skips the download+parse.
	etag         string
	lastModified string
}

// groupKey is the backoff/cap bucket for a target: prefer the ASN org (so a whole
// hoster backs off together), then the resolved IP, then the registrable site
// (collapses *.blogspot.com → blogspot.com for un-enriched fan-out).
func (t target) groupKey() string {
	if t.org != "" {
		return "asn:" + t.org
	}
	if t.ip != "" {
		return "ip:" + t.ip
	}
	if r := crawlutil.Registrable(t.domain); r != "" {
		return "par:" + r
	}
	return "par:" + t.domain
}

// breakState is the per-group rolling failure counter + open-window state.
type breakState struct {
	mu        sync.Mutex
	fails     int
	winStart  time.Time
	openUntil time.Time
	level     int // consecutive trips → exponential cooldown
}

type asnBreaker struct {
	enabled   bool
	threshold int           // fails within window that trip the breaker
	window    time.Duration // rolling failure window
	base      time.Duration // first cooldown; doubles each consecutive trip
	cap       time.Duration // cooldown ceiling
	m         sync.Map      // groupKey -> *breakState
}

var breaker = &asnBreaker{}

// blocked reports whether this group is currently in its cooldown (open) window.
func (b *asnBreaker) blocked(key string) bool {
	if !b.enabled || key == "" {
		return false
	}
	v, ok := b.m.Load(key)
	if !ok {
		return false
	}
	s := v.(*breakState)
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.openUntil)
}

// fail records a timeout/throttle failure for a group; trips the breaker when the
// threshold is reached within the window, opening it for an exponential cooldown.
func (b *asnBreaker) fail(key string) {
	if !b.enabled || key == "" {
		return
	}
	v, _ := b.m.LoadOrStore(key, &breakState{})
	s := v.(*breakState)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Before(s.openUntil) {
		return // already open; keep the current cooldown
	}
	if s.winStart.IsZero() || now.Sub(s.winStart) > b.window {
		s.winStart, s.fails = now, 0
	}
	s.fails++
	if s.fails >= b.threshold {
		s.level++
		cd := b.base << (s.level - 1)
		if cd > b.cap || cd <= 0 { // cooldown pinned at the cap (or the shift overflowed)
			cd = b.cap
			// Do NOT ratchet the level once the cooldown is already at the cap. Otherwise it
			// climbs unbounded (seen live at level 21) while every level at/above this one yields
			// the same cap cooldown, and success()'s 1-per-clean-cycle decay can never unwind it
			// before the group re-trips → the ASN stays locked out effectively forever. Clamping
			// the level here lets a recovered group relax in a bounded number of clean cycles.
			s.level--
		}
		s.openUntil = now.Add(cd)
		s.fails, s.winStart = 0, time.Time{}
		log.Printf("asn-backoff: OPEN %s for %s (level %d)", key, cd, s.level)
		minc(mBreakerOpen)
	}
}

// success records that the group responded; clears the rolling failure count and, after
// a clean result past any cooldown, relaxes the escalation level so it can recover.
func (b *asnBreaker) success(key string) {
	if !b.enabled || key == "" {
		return
	}
	v, ok := b.m.Load(key)
	if !ok {
		return
	}
	s := v.(*breakState)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails, s.winStart = 0, time.Time{}
	if time.Now().After(s.openUntil) && s.level > 0 {
		s.level-- // decay escalation after a clean recovery so cooldowns don't ratchet forever
	}
}

// prune drops breakState entries that are neither open nor recently active, so b.m stays
// bounded to currently-relevant groups instead of growing one entry per distinct ASN/IP/
// parent forever (a slow leak). A pruned group that fails again just gets a fresh entry.
func (b *asnBreaker) prune(maxIdle time.Duration) {
	cut := time.Now().Add(-maxIdle)
	b.m.Range(func(k, v any) bool {
		s := v.(*breakState)
		s.mu.Lock()
		stale := s.openUntil.Before(cut) && (s.winStart.IsZero() || s.winStart.Before(cut))
		s.mu.Unlock()
		if stale {
			b.m.Delete(k)
		}
		return true
	})
}

// isBackoffError classifies transport failures that should count against a group:
// timeouts and connection-level failures (the signature of being rate-blocked or
// pointed at dead infra). Content-level outcomes (404s, challenges) are NOT counted.
func isBackoffError(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	s := strings.ToLower(err.Error())
	// 2026-10-02: + "eof"/"server closed": nginx's 444 (and many WAFs) drop the connection without a response —
	// that is a block, and it never tripped the breaker, so the crawler kept walking the server's other vhosts.
	for _, m := range []string{"timeout", "deadline exceeded", "connection refused", "connection reset", "no route to host", "network is unreachable", "handshake", "i/o timeout", "eof", "server closed", "closed connection"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func envStr(k, d string) string {
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
func envBool(k string, d bool) bool {
	v := strings.ToLower(os.Getenv(k))
	if v == "" {
		return d
	}
	return v == "1" || v == "true" || v == "yes"
}

// ---------------- blacklist (permanent content filter) ----------------
//
// Two rule kinds (cl0q_blacklist.kind): `domain` = an exact apex to remove (foo.com + *.foo.com);
// `keyword` = a banned token matched only at a dot/hyphen LABEL BOUNDARY. The old code used
// strings.Contains for everything, so `cialis` substring-matched "so-CIALIS-t" and would DELETE
// 100+ legit socialist/specialist sites (this crawler deletes matched pages, so the bug was
// destructive), while missing the gambling spam it was for. Boundary matching fixes both.
var (
	blMu      sync.RWMutex
	blDomains map[string]bool
	blWords   []string
)

// loadBlacklist refreshes the in-memory rules from cl0q_blacklist, split by kind.
func loadBlacklist(db *sql.DB) {
	rows, err := db.Query("SELECT lower(keyword), coalesce(kind,'keyword') FROM cl0q_blacklist")
	if err != nil {
		log.Printf("blacklist load: %v", err)
		return
	}
	defer rows.Close()
	doms := map[string]bool{}
	var words []string
	for rows.Next() {
		var k, kind string
		if rows.Scan(&k, &kind) != nil {
			continue
		}
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		if kind == "domain" {
			doms[strings.TrimPrefix(k, "www.")] = true
		} else {
			words = append(words, k)
		}
	}
	if err := rows.Err(); err != nil {
		// A mid-iteration error yields a PARTIAL blacklist — installing it would silently let
		// banned domains through. Keep the previously-loaded rules rather than overwrite with a subset.
		log.Printf("blacklist load: row iteration error (%v) — keeping previous rules", err)
		return
	}
	blMu.Lock()
	blDomains, blWords = doms, words
	blMu.Unlock()
	log.Printf("blacklist: %d domain(s) + %d keyword(s) loaded", len(doms), len(words))
}

// blWordBoundary: term appears as a whole dot/hyphen-delimited token (separator or edge on BOTH
// sides). Blocks slot-x / x-slot / casino-cialis but never a term buried in a longer label
// (socialist, specialist, aeroslot).
func blWordBoundary(hay, term string) bool {
	sep := func(b byte) bool { return b == '.' || b == '-' || b == ' ' }
	from := 0
	for {
		i := strings.Index(hay[from:], term)
		if i < 0 {
			return false
		}
		s := from + i
		e := s + len(term)
		lok := s == 0 || sep(hay[s-1])
		rok := e == len(hay) || sep(hay[e])
		if lok && rok {
			return true
		}
		from = s + 1
	}
}

// blDomainHit: host is an exact blocked domain or a subdomain of one. It walks the host's parent names (a.b.c → b.c → c),
// so a check costs one map lookup per label rather than a scan of the whole blacklist.
func blDomainHit(host string) bool {
	for h := strings.TrimPrefix(strings.ToLower(host), "www."); h != ""; {
		if blDomains[h] {
			return true
		}
		i := strings.IndexByte(h, '.')
		if i < 0 {
			break
		}
		h = h[i+1:]
	}
	return false
}

// isBlacklisted reports whether a crawled page matches any banned keyword
// (case-insensitive substring of domain + title + keywords + description).
func isBlacklisted(domain string, info *WebsiteInfo) (string, bool) {
	blMu.RLock()
	defer blMu.RUnlock()
	if len(blDomains) == 0 && len(blWords) == 0 {
		return "", false
	}
	if blDomainHit(domain) {
		return domain, true
	}
	// Keywords match on a boundary in the domain OR in the page text (title/keywords/description).
	// Space is a separator too, so a keyword still matches a whole word in prose.
	hay := strings.ToLower(domain + " " + info.Title + " " + info.Keywords + " " + info.Description)
	for _, t := range blWords {
		if blWordBoundary(hay, t) {
			return t, true
		}
	}
	return "", false
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	validateConfig() // NR2: refuse to start on any invalid knob, before anything else runs
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	// NR4: single-URL dry-run. Crawls one URL through the real pipeline
	// (robots -> fetch -> parse) and prints exactly what cl0q-node would
	// send for it, writing nothing — no DB needed. The operator's fastest
	// debug loop: SINGLE_URL=https://example.com website-crawler
	if rawURL := strings.TrimSpace(os.Getenv("SINGLE_URL")); rawURL != "" {
		if os.Getenv("CL0Q_NODE_ID") == "" {
			log.Println("warning: CL0Q_NODE_ID not set (dry-run only; set it before crawling for real)")
		}
		info, host, err := fetchOneURLInfo(ctx, rawURL)
		if err != nil {
			log.Fatalf("single-url: %v", err)
		}
		out, err := json.MarshalIndent(dryRunEnvelope(host, info), "", "  ")
		if err != nil {
			log.Fatalf("single-url: marshal: %v", err)
		}
		fmt.Println(string(out))
		fmt.Println("\ndry-run: nothing written, nothing sent")
		return
	}

	dsn := envStr("DATABASE_URL", "postgres://cl0q:cl0q@localhost:5432/cl0q?sslmode=disable")
	// Throughput settings are clamped too: a typo (0, negative, huge) must not stall or flood the crawler. Pacing, not
	// the worker count, is what bounds the load on any one server.
	workers := clampInt(envInt("WORKERS", 16), 1, 64)
	dialSem = make(chan struct{}, clampInt(envInt("MAX_CONNS", 48), 1, 256)) // concurrent HTTP dials
	userAgent = config.PublicUserAgent()                                     // truncated operator prefix, PROTOCOL.md §4
	fetchTimeout = time.Duration(clampInt(envInt("FETCH_TIMEOUT_MS", 20000), 2000, 60000)) * time.Millisecond
	dbConns := clampInt(envInt("DB_MAXCONNS", 10), 1, 50)
	loadPoliteness() // pacing, retries and circuit breaker, clamped to the politeness floors
	// Verify TLS certs by default (don't trust MITM'd/spoofed content, and don't behave like a
	// scanner probing hosts regardless of cert validity). CRAWLER_TLS_INSECURE=1 restores the old
	// skip-verify behavior if it's dropping too many legit sites with expired/self-signed certs.
	if envBool("CRAWLER_TLS_INSECURE", false) {
		transport.TLSClientConfig.InsecureSkipVerify = true
		log.Printf("crawler: TLS verification DISABLED (CRAWLER_TLS_INSECURE=1)")
	}
	// ASN/IP circuit breaker (always on): when a group (ASN org → IP → registrable parent) fails
	// breaker.threshold times within the window, its targets are deferred for an exponential cooldown
	// so rate-blocked or dead infra isn't hammered.
	log.Printf("politeness: host_delay=%s ip_delay=%s max_per_group=%d recrawl_days=%d throttle_retries=%d retry_wait_max=%s breaker threshold=%d window=%s cooldown=%s..%s pool_factor=%d",
		hostDelay, ipDelay, perGroupCap, recrawlDays(), throttleRetries, throttleWaitMax,
		breaker.threshold, breaker.window, breaker.base, breaker.cap, envInt("ASN_POOL_FACTOR", 3))
	dbBatch := envInt("DB_BATCH", 200) // crawl results per DB transaction (multi-row upsert) — fewer commits = far less remote WAL/fsync load
	if dbBatch < 1 {
		dbBatch = 1
	} else if dbBatch > 2000 {
		dbBatch = 2000 // keep bind-param count (×23 cols) well under Postgres' 65535 limit
	}
	// Optional local outbox: when the database is down, spool batches to SPOOL_DATABASE_URL instead of dropping
	// them, for a separate drain job to replay. Not used by the node build (nil = disabled when unset).
	spoolCap := envInt("SPOOL_MAX_ROWS", 500000)
	spool := dbresil.Open(os.Getenv("SPOOL_DATABASE_URL"), spoolCap)
	if spool != nil {
		log.Printf("spool: local outbox enabled (cap %d rows)", spoolCap)
	}

	db, err := sql.Open("postgres", dbresil.WithConnectTimeout(dsn, 10))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	// Upserts run on ONE consumer goroutine (see crawlBatch), so the crawler needs
	// only a few DB conns regardless of worker count. Keep the pool small to stay
	// well clear of the shared 100-conn budget (was workers+16 = 48).
	db.SetMaxOpenConns(dbConns)
	fpChecker = footprint.New(db)                  // NR3: bounded footprint
	sitemapFetch = envBool("SITEMAP_FETCH", false) // IG3: fetch /sitemap.xml for recrawl signals (opt-in, doubles requests)
	// NR6: observability.
	mreg = metrics.NewRegistry()
	mCrawledOK = mreg.Counter("crawler_pages_ok_total", "pages successfully crawled")
	mCrawledFailed = mreg.Counter("crawler_pages_failed_total", "pages where the fetch failed")
	mCrawledSkipped = mreg.Counter("crawler_pages_skipped_total", "pages skipped (robots, blacklist note)")
	mCrawledChallenged = mreg.Counter("crawler_pages_challenged_total", "pages behind a bot challenge")
	mCrawledUnchanged = mreg.Counter("crawler_pages_unchanged_total", "pages unchanged (304)")
	mCrawledDuplicates = mreg.Counter("crawler_pages_duplicates_total", "IG2: pages detected as near-duplicates (SimHash)")
	mSitemapFound = mreg.Counter("crawler_sitemaps_found_total", "IG3: sitemaps successfully parsed")
	mCrawledDeferred = mreg.Counter("crawler_pages_deferred_total", "pages deferred by ASN/IP backoff")
	mCrawledBlocked = mreg.Counter("crawler_pages_blocked_total", "pages blocked by blacklist")
	mBreakerOpen = mreg.Counter("crawler_breaker_opens_total", "times the ASN/IP breaker opened a cooldown")
	mSitemapURLs = mreg.Counter("crawler_sitemap_urls_total", "T1: domains discovered via sitemaps")
	mreg.GaugeFunc("crawler_frontier_depth", "uncrawled domains in domain_info", func() int64 {
		var n int64
		_ = db.QueryRow(`SELECT count(*) FROM domain_info WHERE web_crawled_at IS NULL`).Scan(&n)
		return n
	})
	mreg.GaugeFunc("crawler_db_domains", "rows in domain_info", func() int64 {
		var n int64
		_ = db.QueryRow(`SELECT count(*) FROM domain_info`).Scan(&n)
		return n
	})
	mreg.GaugeFunc("crawler_db_pages", "rows in website_info", func() int64 {
		var n int64
		_ = db.QueryRow(`SELECT count(*) FROM website_info`).Scan(&n)
		return n
	})
	mreg.GaugeFunc("crawler_breaker_open_groups", "groups currently in breaker cooldown", func() int64 {
		var n int64
		breaker.m.Range(func(_, _ any) bool { n++; return true })
		return n
	})
	if addr := os.Getenv("METRICS_ADDR"); addr != "" {
		mreg.Serve(addr)
		log.Printf("metrics: serving /metrics and /status on %s", addr)
	}
	db.SetMaxIdleConns(min(4, dbConns))
	db.SetConnMaxLifetime(5 * time.Minute)
	if dbresil.WaitForRemoteCtx(ctx, db) != nil { // wait out a database outage at startup instead of crash-looping
		return
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS website_info (
			id SERIAL PRIMARY KEY, domain TEXT NOT NULL UNIQUE, title TEXT, description TEXT,
			keywords TEXT, canonical TEXT, h1 TEXT, status_code INT, content_type TEXT,
			content_length BIGINT, latency_ms BIGINT, redirects INT, outbound_links INT,
			image_count INT, word_count INT, charset TEXT, robots TEXT, og_title TEXT,
			og_image TEXT, twitter_card TEXT, page_language TEXT, server_header TEXT,
			fetch_error TEXT, etag TEXT, last_modified TEXT,
			crawled_at TIMESTAMP NOT NULL DEFAULT NOW())`); err != nil {
		log.Fatalf("ensure table: %v", err)
	}
	_, _ = db.Exec(`ALTER TABLE website_info ADD COLUMN IF NOT EXISTS fetch_error TEXT`)
	// body_text intentionally NOT (re)created — it's unused by search and was dropped to save space.
	// Conditional-GET validators: stored from each 200 so the next recrawl can ask "changed since?"
	// and skip the download+parse when the server answers 304.
	_, _ = db.Exec(`ALTER TABLE website_info ADD COLUMN IF NOT EXISTS etag TEXT`)
	_, _ = db.Exec(`ALTER TABLE website_info ADD COLUMN IF NOT EXISTS last_modified TEXT`)
	// source_node tracks which node first authored the row (used by "purge a bad
	// node" deletes). The upserts reference it, so a binary-created DB (no
	// docker initdb) must have the column too — otherwise prepare fails at
	// startup.
	_, _ = db.Exec(`ALTER TABLE website_info ADD COLUMN IF NOT EXISTS source_node TEXT`)
	// IG2: SimHash fingerprint for near-duplicate detection. The 64-bit hash
	// is split into 4x16-bit blocks for indexed lookup (near-dupes share a block).
	_, _ = db.Exec(`ALTER TABLE website_info ADD COLUMN IF NOT EXISTS simhash BIGINT`)
	_, _ = db.Exec(`ALTER TABLE website_info ADD COLUMN IF NOT EXISTS simhash_b0 SMALLINT`)
	_, _ = db.Exec(`ALTER TABLE website_info ADD COLUMN IF NOT EXISTS simhash_b1 SMALLINT`)
	_, _ = db.Exec(`ALTER TABLE website_info ADD COLUMN IF NOT EXISTS simhash_b2 SMALLINT`)
	_, _ = db.Exec(`ALTER TABLE website_info ADD COLUMN IF NOT EXISTS simhash_b3 SMALLINT`)
	_, _ = db.Exec(`ALTER TABLE website_info ADD COLUMN IF NOT EXISTS duplicate_of TEXT`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_website_info_simhash_b0 ON website_info (simhash_b0)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_website_info_simhash_b1 ON website_info (simhash_b1)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_website_info_simhash_b2 ON website_info (simhash_b2)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_website_info_simhash_b3 ON website_info (simhash_b3)`)

	// Upsert refreshes data + crawled_at on re-crawl.
	upsert, err := db.Prepare(`
		INSERT INTO website_info (
			domain, title, description, keywords, canonical, h1, status_code, content_type,
			content_length, latency_ms, redirects, outbound_links, image_count, word_count,
			charset, robots, og_title, og_image, twitter_card, page_language, server_header,
			fetch_error, etag, last_modified, simhash, simhash_b0, simhash_b1, simhash_b2,
			simhash_b3, duplicate_of, crawled_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,NOW())
		ON CONFLICT (domain) DO UPDATE SET
			title=EXCLUDED.title, description=EXCLUDED.description, keywords=EXCLUDED.keywords,
			canonical=EXCLUDED.canonical, h1=EXCLUDED.h1, status_code=EXCLUDED.status_code,
			content_type=EXCLUDED.content_type, content_length=EXCLUDED.content_length,
			latency_ms=EXCLUDED.latency_ms, redirects=EXCLUDED.redirects,
			outbound_links=EXCLUDED.outbound_links, image_count=EXCLUDED.image_count,
			word_count=EXCLUDED.word_count, charset=EXCLUDED.charset, robots=EXCLUDED.robots,
			og_title=EXCLUDED.og_title, og_image=EXCLUDED.og_image, twitter_card=EXCLUDED.twitter_card,
			page_language=EXCLUDED.page_language, server_header=EXCLUDED.server_header,
			fetch_error=EXCLUDED.fetch_error, etag=EXCLUDED.etag, last_modified=EXCLUDED.last_modified,
	simhash=EXCLUDED.simhash, simhash_b0=EXCLUDED.simhash_b0, simhash_b1=EXCLUDED.simhash_b1,
	simhash_b2=EXCLUDED.simhash_b2, simhash_b3=EXCLUDED.simhash_b3, duplicate_of=EXCLUDED.duplicate_of,
	sitemap_urls=EXCLUDED.sitemap_urls, sitemap_lastmod=EXCLUDED.sitemap_lastmod,
			crawled_at=NOW(),
			-- This crawler re-authored the row, so it reclaims provenance (like the batch
			-- upsertTail). Without it, a row re-crawled via the row-by-row FALLBACK keeps a stale
			-- source_node and a "purge a bad node" DELETE WHERE source_node='x' could nuke it.
			source_node=NULL`)
	if err != nil {
		log.Fatalf("prepare upsert: %v", err)
	}
	defer upsert.Close()

	// touchStmt refreshes only crawled_at for a 304 Not Modified result — the stored content
	// is still current, so we advance the recrawl clock without rewriting the row.
	touchStmt, err := db.Prepare(`UPDATE website_info SET crawled_at = NOW() WHERE domain = $1`)
	if err != nil {
		log.Fatalf("prepare touch: %v", err)
	}
	defer touchStmt.Close()

	// ---------------- blacklist (permanent content filter) ----------------
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS cl0q_blacklist (
		id SERIAL PRIMARY KEY, keyword TEXT NOT NULL UNIQUE, note TEXT,
		added_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		log.Printf("ensure cl0q_blacklist: %v", err)
	}
	// loadBlacklist queries coalesce(kind,'keyword'); the binary's own CREATE
	// TABLE above never created the column, so fresh installs silently ran with
	// an empty blacklist.
	_, _ = db.Exec(`ALTER TABLE cl0q_blacklist ADD COLUMN IF NOT EXISTS kind TEXT`)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS cl0q_settings (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		log.Printf("ensure cl0q_settings: %v", err)
	}
	// dns_fail_count (maintained by cl0q-dns-enricher): skip domains DNS can't resolve after 3 spaced tries.
	if _, err := db.Exec(`ALTER TABLE domain_info ADD COLUMN IF NOT EXISTS dns_fail_count INT NOT NULL DEFAULT 0`); err != nil {
		log.Printf("ensure dns_fail_count: %v", err)
	}
	// T2: OPIC scoring (streaming PageRank). Each domain holds "cash"; crawling
	// distributes it to outlinks, resets to zero, and accumulates "history".
	// Score = history + cash; the frontier orders by score DESC.
	if _, err := db.Exec(`ALTER TABLE domain_info ADD COLUMN IF NOT EXISTS opic_cash REAL NOT NULL DEFAULT 1.0`); err != nil {
		log.Printf("ensure opic_cash: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE domain_info ADD COLUMN IF NOT EXISTS opic_history REAL NOT NULL DEFAULT 0`); err != nil {
		log.Printf("ensure opic_history: %v", err)
	}
	// #81: Use CREATE INDEX CONCURRENTLY — plain CREATE INDEX takes ACCESS EXCLUSIVE
	// on domain_info (13M rows), blocking the crawler for minutes. CONCURRENTLY
	// builds without blocking reads/writes. It cannot run in a transaction block,
	// but db.Exec here is autocommit (not tx.Exec), so this is safe.
	// A failed prior CONCURRENTLY leaves an INVALID index; detect and rebuild it.
	var idxValid *bool
	_ = db.QueryRow(`SELECT indisvalid FROM pg_index WHERE indexrelid = 'idx_domain_info_opic_score'::regclass`).Scan(&idxValid)
	if idxValid != nil && !*idxValid {
		log.Printf("ensure idx_domain_info_opic_score: dropping INVALID index for rebuild")
		if _, err := db.Exec(`DROP INDEX idx_domain_info_opic_score`); err != nil {
			log.Printf("ensure idx_domain_info_opic_score drop: %v", err)
		}
		idxValid = nil // force rebuild below
	}
	if idxValid == nil {
		if _, err := db.Exec(`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_domain_info_opic_score ON domain_info ((opic_history + opic_cash) DESC) WHERE web_crawled_at IS NULL AND dns_fail_count < 3`); err != nil {
			log.Printf("ensure idx_domain_info_opic_score: %v", err)
		}
	}
	// T3: adaptive recrawl interval. Per-domain interval in days; NULL means
	// "use the global RECRAWL_DAYS". 304 (unchanged) backs off ×1.25 (cap 30d);
	// 200 (changed) shortens ×0.75 (floor 1d).
	if _, err := db.Exec(`ALTER TABLE domain_info ADD COLUMN IF NOT EXISTS recrawl_interval_days REAL`); err != nil {
		log.Printf("ensure recrawl_interval_days: %v", err)
	}
	delStmt, err := db.Prepare(`DELETE FROM website_info WHERE domain = $1`)
	if err != nil {
		log.Fatalf("prepare delete: %v", err)
	}
	defer delStmt.Close()

	// stampStmt records a crawl ATTEMPT on domain_info (any outcome). This is the
	// durable "we visited this" marker selectTargets keys on under USE_CRAWL_STATE,
	// so blacklist-blocked / non-200-purged domains (whose website_info row is
	// removed) are not re-served forever. No-op for domains not in domain_info.
	stampStmt, err := db.Prepare(`UPDATE domain_info SET web_crawled_at = now() WHERE domain = $1`)
	if err != nil {
		log.Fatalf("prepare stamp: %v", err)
	}
	defer stampStmt.Close()

	// EMIT_DOMAINS (default on): feed the external domains found on each crawled page
	// back into domain_info (dedup via NOT EXISTS) — near-free discovery that reuses
	// the page fetch we already did, instead of re-fetching domains just to harvest
	// links. Inserts domain-only rows (no IP yet); selectTargets then crawls them and
	// asn-enricher fills IPs once resolved. Set EMIT_DOMAINS=0 to disable.
	var emitStmt *sql.Stmt
	if envBool("EMIT_DOMAINS", true) {
		emitStmt, err = db.Prepare(`INSERT INTO domain_info (domain, found_on, crawled_at)
			SELECT d, $1, now() FROM unnest($2::text[]) AS d
			WHERE NOT EXISTS (SELECT 1 FROM domain_info di WHERE di.domain = d)`)
		if err != nil {
			log.Fatalf("prepare emit: %v", err)
		}
		defer emitStmt.Close()
	}

	loadBlacklist(db)
	go func() { // refresh every 60s so admin blacklist edits take effect live
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for range t.C {
			loadBlacklist(db)
		}
	}()

	loop := envBool("LOOP", false)
	log.Printf("crawler start: workers=%d dials=%d db_pool=%d db_batch=%d timeout=%s host_delay=%s loop=%v emit=%v ua=%q",
		workers, cap(dialSem), dbConns, dbBatch, fetchTimeout, hostDelay, loop, emitStmt != nil, userAgent)

	// PIPELINE=1: continuous pipelined crawl (no per-batch barrier — see crawlContinuous).
	// Only meaningful in LOOP mode; it runs forever. Default off → the proven batch path below.
	if loop && envBool("PIPELINE", false) {
		crawlContinuous(ctx, db, spool, workers, dbBatch, upsert, delStmt, emitStmt, stampStmt, touchStmt)
		log.Println("stopped: shutdown signal received, in-flight results written")
		return
	}

	for ctx.Err() == nil {
		crawlSubdomains.Store(crawlutil.LoadCrawlSubdomains(db, crawlSubdomains.Load())) // pick up the operator's toggle each cycle
		targets, err := selectTargets(db)
		if err != nil {
			log.Printf("select targets failed (%v) — waiting for the remote DB to return", err)
			if dbresil.WaitForRemoteCtx(ctx, db) != nil {
				break
			}
			continue // remote is back; retry from the top
		}
		if len(targets) == 0 {
			if !loop {
				log.Println("no targets — exiting (one-shot run: LOOP is off)")
				return
			}
			log.Println("no work — sleeping 30s")
			dbresil.SleepCtx(ctx, 30*time.Second)
			continue
		}
		crawlBatch(ctx, db, spool, targets, workers, dbBatch, upsert, delStmt, emitStmt, stampStmt, touchStmt)
		if !loop {
			log.Println("batch complete — exiting (one-shot run: LOOP is off)")
			return
		}
	}
	log.Println("stopped: shutdown signal received, in-flight results written")
}

// selectTargets resolves the work set from TARGET / SAMPLE / default (new + stale).
// For the default path it OVER-selects a pool (BATCH × ASN_POOL_FACTOR) via random page
// sampling (TABLESAMPLE, FRONTIER_SAMPLE_PCT) so the pool reflects the true ASN mix of the
// frontier rather than a physically-clustered slice, enriches each domain with its first
// IPv4 + ASN org, then de-clusters the batch (capBatch) so one ASN/IP can't monopolize it
// and breaker-open groups are skipped up front. The sampling is load-bearing: without it the
// clustered draw hands capBatch a single-ASN pool it cannot diversify (see FRONTIER_SAMPLE_PCT).
func selectTargets(db *sql.DB) ([]target, error) {
	if t := strings.TrimSpace(os.Getenv("TARGET")); t != "" {
		d, err := parseTarget(t)
		if err != nil {
			log.Fatalf("TARGET: %v", err)
		}
		return []target{{domain: d}}, nil
	}
	if n := envInt("SAMPLE", 0); n > 0 {
		// TABLESAMPLE is fast random-ish (avoids ORDER BY random() full sort).
		rows, err := db.Query(`SELECT domain, etag, last_modified FROM website_info TABLESAMPLE SYSTEM (0.2) WHERE domain IS NOT NULL LIMIT $1`, n)
		if err != nil {
			return nil, err
		}
		return scanTargets(rows, false)
	}
	batch := envInt("BATCH", 500)
	recrawl := recrawlDays()
	maxPerBatch := perGroupCap
	poolFactor := envInt("ASN_POOL_FACTOR", 3)
	if poolFactor < 1 {
		poolFactor = 1
	}
	pool := batch * poolFactor
	// FRONTIER_SAMPLE_PCT: TABLESAMPLE % for the uncrawled-frontier draw. The frontier is
	// physically clustered by ASN — bulk CT-seed inserts land contiguously, and rows whose
	// group is breaker-open get deferred forever (never web_crawled_at-stamped), so they pile
	// up at the front of the scan. A plain `LIMIT pool` then returns a near-single-ASN slice
	// (measured live: 3999/4000 = one ASN vs. that ASN's true ~3% share of the frontier), so
	// when that ASN's breaker opens the WHOLE pool defers → 0 targets → the crawler stalls
	// while millions of crawlable domains on other ASNs sit unreachable behind the LIMIT.
	// Random page sampling makes the pool representative of the true (diverse) frontier, so a
	// blocked mega-ASN only costs its real share. 0.5% of ~20M rows ≈ 100k sampled → fills the
	// pool while reading ~1% of the table (same cost class as the SAMPLE/recrawl TABLESAMPLE).
	samplePct := 0.5
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("FRONTIER_SAMPLE_PCT")), 64); err == nil && v > 0 && v <= 100 {
		samplePct = v
	}
	// `picked` selects (domain, first-ipv4) so the outer query needs only the cheap
	// asninfo join (on ip, its PK) — no domain_info self-join. Empty ip → org falls back
	// to the registrable-parent grouping in groupKey.
	if envBool("USE_CRAWL_STATE", false) && envBool("FRONTIER_SNAPSHOT", true) {
		return selectFromSnapshot(db, pool, recrawl, maxPerBatch, batch*2)
	}
	var picked string
	if envBool("USE_CRAWL_STATE", false) {
		// Crawl-state model (key on domain_info.web_crawled_at, stamped on EVERY attempt):
		// a domain is eligible if never attempted, or due for refresh. Survives blacklist
		// deletes + non-200 purges, so junk isn't re-served forever.
		picked = fmt.Sprintf(`
			(SELECT domain, coalesce(btrim(split_part(ip_addresses_ipv4, ',', 1)), '') AS ip
			   FROM domain_info TABLESAMPLE SYSTEM (%g) WHERE web_crawled_at IS NULL AND dns_fail_count < 3 LIMIT %d)
			UNION
			(SELECT domain, coalesce(btrim(split_part(ip_addresses_ipv4, ',', 1)), '') AS ip
			   FROM domain_info WHERE web_crawled_at < NOW() - INTERVAL '%d days' AND dns_fail_count < 3 LIMIT %d)`, samplePct, pool, recrawl, pool)
	} else {
		// Legacy model: "uncrawled" = in domain_info but missing from website_info. The stale
		// set comes from website_info (no ip column) → ip '' → parent grouping for those.
		picked = fmt.Sprintf(`
			(SELECT di.domain, coalesce(btrim(split_part(di.ip_addresses_ipv4, ',', 1)), '') AS ip
			   FROM domain_info di TABLESAMPLE SYSTEM (%g) LEFT JOIN website_info wi ON di.domain = wi.domain
			   WHERE wi.domain IS NULL AND di.dns_fail_count < 3 LIMIT %d)
			UNION
			(SELECT domain, ''::text AS ip
			   FROM website_info WHERE crawled_at < NOW() - INTERVAL '%d days' LIMIT %d)`, samplePct, pool, recrawl, pool)
	}
	// wi join is a per-pooled-row lookup on website_info's UNIQUE(domain) — cheap — and pulls the
	// stored validators so recrawls can go conditional (NULL etag/last_modified → full fetch).
	q := `WITH picked AS (` + picked + `)
		SELECT p.domain, p.ip, coalesce(a.org, ''), wi.etag, wi.last_modified
		  FROM picked p
		  LEFT JOIN asninfo a ON a.ip = nullif(p.ip, '')
		  LEFT JOIN website_info wi ON wi.domain = p.domain`
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	ts, err := scanTargets(rows, true)
	if err != nil {
		return nil, err
	}
	// Return up to the original ceiling (batch new + batch stale), de-clustered.
	return capBatch(ts, maxPerBatch, batch*2), nil
}

// ---- frontier snapshot (2026-09-28) ----
// The crawl-state frontier (domain_info.web_crawled_at IS NULL AND dns_fail_count < 3) is small and nearly static —
// measured 2026-09-28: ~119k rows across 394 orgs, ~97% of them on groups whose breaker is open at any moment, while
// new domains are crawled within minutes. The TABLESAMPLE 0.5% draw above re-read ~195k rows (2–4 s of DB) per
// select to find ~550 frontier rows (~19 crawlable), ~12x/min: ~40–80% of the 1-core DB. Instead the whole frontier
// (domain, first IPv4, ASN org) is read in ONE pass over the partial index idx_domain_info_crawl_todo every
// FRONTIER_REFRESH_S (default 300 s; ~1 s warm), shuffled, and each select draws from memory — skipping groups whose
// breaker is open BEFORE they reach the pool. A drawn row leaves the snapshot (crawled → stamped; if it ends up
// deferred it simply returns at the next reload). The recrawl-due set stays a (cheap, indexed) query.
// Kill switch: FRONTIER_SNAPSHOT=0 restores the TABLESAMPLE draw with no rebuild.
type frontierSnapshot struct {
	mu     sync.Mutex
	rows   []target
	keys   []string // groupKey() of rows[i], precomputed at load
	cursor int
	loaded time.Time
}

var frontierSnap frontierSnapshot

// loadFrontier reads the whole frontier (highest OPIC score first, capped at
// max rows) with its breaker group key. T2: OPIC ordering replaces the old
// newest-first order — domains that accumulate cash from inlinks (i.e. sites
// many others link to) are crawled before isolated ones.
func loadFrontier(db *sql.DB, max int) ([]target, []string, error) {
	rows, err := db.Query(`SELECT d.domain, coalesce(btrim(split_part(d.ip_addresses_ipv4, ',', 1)), ''), coalesce(a.org, '')
		  FROM domain_info d
		  LEFT JOIN asninfo a ON a.ip = nullif(btrim(split_part(d.ip_addresses_ipv4, ',', 1)), '')
		 WHERE d.web_crawled_at IS NULL AND d.dns_fail_count < 3
		 ORDER BY (d.opic_history + d.opic_cash) DESC, d.id DESC LIMIT $1`, max)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var ts []target
	var keys []string
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.domain, &t.ip, &t.org); err != nil {
			return nil, nil, err
		}
		ts = append(ts, t)
		keys = append(keys, t.groupKey())
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	// T2: no shuffle — the OPIC score order IS the priority. Diversity across
	// breaker groups is enforced downstream by draw()'s maxPerGroup caps and
	// capBatch's de-clustering.
	return ts, keys, nil
}

// draw returns up to n snapshot rows whose group breaker is closed (at most maxPerGroup per group, 0 = no cap),
// removing them from the snapshot; rows of breaker-open or already-full groups are skipped and stay for a later
// draw. Reloads the snapshot when it's older than refresh.
func (f *frontierSnapshot) draw(db *sql.DB, n, maxPerGroup int, refresh time.Duration, max int) ([]target, error) {
	f.mu.Lock()
	stale := f.loaded.IsZero() || time.Since(f.loaded) > refresh
	f.mu.Unlock()
	if stale {
		t0 := time.Now()
		ts, keys, err := loadFrontier(db, max)
		if err != nil {
			return nil, err
		}
		groups := map[string]bool{}
		for _, k := range keys {
			groups[k] = true
		}
		log.Printf("frontier snapshot: %d rows, %d groups loaded in %s (cap %d)", len(ts), len(groups), time.Since(t0).Round(time.Millisecond), max)
		f.mu.Lock()
		f.rows, f.keys, f.cursor, f.loaded = ts, keys, 0, time.Now()
		f.mu.Unlock()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]target, 0, n)
	perGroup := map[string]int{}
	i, total := f.cursor, len(f.rows)
	for visited := 0; visited < total && len(out) < n && len(f.rows) > 0; visited++ {
		if i >= len(f.rows) {
			i = 0
		}
		if k := f.keys[i]; breaker.blocked(k) || (maxPerGroup > 0 && perGroup[k] >= maxPerGroup) {
			i++
			continue
		}
		perGroup[f.keys[i]]++
		out = append(out, f.rows[i])
		last := len(f.rows) - 1 // swap-remove; the (pre-shuffled) last row takes slot i
		f.rows[i], f.keys[i] = f.rows[last], f.keys[last]
		f.rows, f.keys = f.rows[:last], f.keys[:last]
	}
	f.cursor = i
	return out, nil
}

// selectFromSnapshot = the crawl-state select with the frontier drawn from the in-memory snapshot (see above).
func selectFromSnapshot(db *sql.DB, pool, recrawl, maxPerBatch, maxReturn int) ([]target, error) {
	t0 := time.Now()
	fresh, err := frontierSnap.draw(db, maxReturn, maxPerBatch, time.Duration(envInt("FRONTIER_REFRESH_S", 300))*time.Second, envInt("FRONTIER_MAX", 400000))
	if err != nil {
		return nil, err
	}
	// Recrawl-due rows: an index range scan on idx_domain_info_web_crawled_at (empty until the first rows age past
	// their interval), with the stored validators for conditional GETs.
	// T3: per-domain adaptive interval (recrawl_interval_days, NULL = global RECRAWL_DAYS).
	rows, err := db.Query(`WITH picked AS (
			SELECT domain, coalesce(btrim(split_part(ip_addresses_ipv4, ',', 1)), '') AS ip
			  FROM domain_info WHERE web_crawled_at < NOW() - make_interval(days => COALESCE(recrawl_interval_days, $1)) AND dns_fail_count < 3 LIMIT $2)
		SELECT p.domain, p.ip, coalesce(a.org, ''), wi.etag, wi.last_modified
		  FROM picked p
		  LEFT JOIN asninfo a ON a.ip = nullif(p.ip, '')
		  LEFT JOIN website_info wi ON wi.domain = p.domain`, recrawl, pool)
	if err != nil {
		return nil, err
	}
	stale, err := scanTargets(rows, true)
	if err != nil {
		return nil, err
	}
	frontierSnap.mu.Lock()
	left := len(frontierSnap.rows)
	frontierSnap.mu.Unlock()
	log.Printf("select: snapshot drew %d fresh (%d left) + %d recrawl in %s", len(fresh), left, len(stale), time.Since(t0).Round(time.Millisecond))
	return capBatch(append(fresh, stale...), maxPerBatch, maxReturn), nil
}

// scanTargets reads (domain[, ip, org]) rows into targets. withInfra selects the enriched
// 3-column shape; otherwise a bare domain list (SAMPLE mode).
func scanTargets(rows *sql.Rows, withInfra bool) ([]target, error) {
	defer rows.Close()
	var out []target
	for rows.Next() {
		var t target
		var etag, lastMod sql.NullString
		if withInfra {
			if err := rows.Scan(&t.domain, &t.ip, &t.org, &etag, &lastMod); err != nil {
				return nil, err
			}
		} else {
			if err := rows.Scan(&t.domain, &etag, &lastMod); err != nil {
				return nil, err
			}
		}
		t.etag, t.lastModified = etag.String, lastMod.String
		out = append(out, t)
	}
	return out, rows.Err()
}

// capBatch de-clusters the enriched pool: walk it IN ORDER (OPIC score order
// from the frontier — the priority), admit at most maxPerGroup targets
// per group (ASN org → IP → registrable parent), skip groups whose breaker is open, and
// dedup domains — up to maxReturn total. This stops one busy/blocked ASN from monopolizing
// a batch (the insert-dip root cause) and yields a diverse, healthy work set.
func capBatch(ts []target, maxPerGroup, maxReturn int) []target {
	if maxReturn < 1 {
		maxReturn = len(ts)
	}
	// Size the maps for the admitted set (at most maxReturn), not the pool: a
	// pool much larger than maxReturn would over-allocate both maps here.
	hint := maxReturn
	if hint > len(ts) {
		hint = len(ts)
	}
	counts := make(map[string]int, hint)
	seenDom := make(map[string]bool, hint)
	out := make([]target, 0, maxReturn)
	var blocked, capped int
	for _, t := range ts {
		if len(out) >= maxReturn {
			break
		}
		if seenDom[t.domain] {
			continue // domain_info can hold duplicate domain rows; crawl each once
		}
		k := t.groupKey()
		if breaker.blocked(k) {
			blocked++
			continue
		}
		if maxPerGroup > 0 && counts[k] >= maxPerGroup {
			capped++
			continue
		}
		counts[k]++
		seenDom[t.domain] = true
		out = append(out, t)
	}
	log.Printf("select: pool=%d -> %d targets (cap %d/group; deferred %d breaker-open, %d over-cap)",
		len(ts), len(out), maxPerGroup, blocked, capped)
	return out
}

// crawlBatch fetches all domains with a worker pool, then persists results in batched
// transactions (see flushBatch) to keep remote-Postgres commit/WAL pressure low. When ctx ends no further target is
// handed out; fetches already in flight finish and are written.
func crawlBatch(ctx context.Context, db *sql.DB, spool *dbresil.Spooler, targets []target, workers, dbBatch int, upsert, delStmt, emitStmt, stampStmt, touchStmt *sql.Stmt) {
	total := len(targets)
	log.Printf("crawling %d domain(s)", total)

	// T4: front/back queue split. Instead of shuffling targets and letting workers
	// sleep in hostThrottle, group by host (front queues) and only hand out
	// targets whose host is already eligible. Workers never block on politeness;
	// the throttle gates remain as a safety net.
	// (The old rand.Shuffle is gone: OPIC score order from the frontier is the priority.)
	fq := newFrontQueue(targets, hostDelay)
	pruneGates(&hostGates, gateIdle)
	pruneGates(&ipGates, gateIdle)
	// robotsCache is keyed per DOMAIN, but every domain is crawled ~once (web_crawled_at),
	// so it never gets a cache hit across batches — it just grew unbounded (one entry per
	// domain, forever) and was the crawler's main memory LEAK (631MB RSS over ~6d uptime →
	// GC thrash + box swap → throughput collapse, 2026-07-13). Reset it per batch; it only
	// needs to dedup within a batch, which bounds it to this batch's domains.
	robotsCache = sync.Map{}
	// Prune sitemapChecked per batch too: entries older than the recheck
	// interval are dead weight, and without pruning the map grows one entry
	// per crawled domain forever.
	pruneSitemapChecked()
	// Prune breaker entries idle past ~2× the max cooldown so its map stays bounded to
	// currently-relevant groups instead of accumulating one entry per ASN/IP/parent forever.
	breaker.prune(2 * breaker.cap)

	jobs := make(chan target, workers*2)
	results := make(chan result, workers*2)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				results <- safeCrawl(t)
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()
	// T4: feed jobs from the front/back queue — only eligible hosts are handed
	// out. When no host is eligible, wait until the next one becomes eligible
	// instead of handing out work that would make a worker sleep.
	go func() {
		defer close(jobs)
		for {
			if t, ok := fq.next(time.Now()); ok {
				select {
				case jobs <- t:
				case <-ctx.Done():
					return // shutting down: hand out nothing more; the rest stay unstamped for the next run
				}
				continue
			}
			wait := fq.waitDuration(time.Now())
			if wait < 0 {
				return // all queues drained
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
		}
	}()

	verbose := total <= 50 || envBool("VERBOSE", false)
	var done, failed, skipped, blocked, challenged, discovered, deferred, unchanged int

	// Buffer results and write them in batched transactions of dbBatch rows. Each flush is ONE
	// tx: a multi-row upsert + a single delete/stamp/emit — vs. ~3 autocommit statements per
	// domain before. That collapse is the main reduction in remote WAL/fsync pressure.
	batch := make([]crawlOut, 0, dbBatch)
	flush := func() {
		discovered += flushBatch(db, spool, batch, upsert, delStmt, stampStmt, emitStmt, touchStmt)
		batch = batch[:0]
	}

	for r := range results {
		if r.deferred {
			deferred++
			minc(mCrawledDeferred)
			if verbose {
				log.Printf("  · %-32s deferred (asn/ip backoff)", r.domain)
			}
			continue // no upsert, no stamp — left uncrawled so a later cycle retries it
		}
		out, oc, term := classify(r, emitStmt != nil)
		switch oc {
		case outSkipped:
			skipped++
			minc(mCrawledSkipped)
			if verbose {
				log.Printf("  · %-32s skip (%s)", r.domain, r.note)
			}
		case outFailed:
			failed++
			minc(mCrawledFailed)
			if verbose {
				log.Printf("  · %-32s ERROR %v", r.domain, r.err)
			}
		case outUnchanged:
			unchanged++
			minc(mCrawledUnchanged)
			if verbose {
				log.Printf("  · %-32s 304  unchanged (%5dms)", r.domain, out.info.LatencyMs)
			}
		case outChallenged:
			challenged++
			minc(mCrawledChallenged)
			if verbose {
				log.Printf("  · %-32s %3d  CHALLENGE (%s)", r.domain, out.info.StatusCode, out.info.Challenge)
			}
		default:
			done++
			minc(mCrawledOK)
			if verbose {
				log.Printf("  · %-32s %3d  %5dms  %q", r.domain, out.info.StatusCode, out.info.LatencyMs, clip(out.info.Title, 50))
			}
		}
		if out.bad {
			blocked++
			minc(mCrawledBlocked)
			if verbose {
				log.Printf("  · %-32s BLOCKED (matched %q)", r.domain, term)
			}
		}
		batch = append(batch, out)
		if len(batch) >= dbBatch {
			flush()
		}
		if (done+failed+skipped+blocked+challenged+unchanged)%50 == 0 {
			log.Printf("progress: %d/%d (ok=%d unchanged=%d fail=%d skip=%d blocked=%d challenge=%d)", done+failed+skipped+blocked+challenged+unchanged, total, done, unchanged, failed, skipped, blocked, challenged)
		}
	}
	flush() // final partial batch

	log.Printf("batch done: %d ok, %d unchanged(304), %d failed, %d skipped, %d blocked, %d challenged, %d deferred, %d new domains (of %d)", done, unchanged, failed, skipped, blocked, challenged, deferred, discovered, total)
}

// crawlContinuous is the PIPELINED crawl loop (PIPELINE=1), an alternative to the batch path.
// A persistent worker pool + one consumer are fed by a producer that refills from selectTargets
// as the queue drains — so there is NO per-batch barrier. Workers that finish a fast page
// immediately pick up the next target instead of idling until the whole batch's slow/timeout
// stragglers complete (the batch-tail idle that capped crawlBatch at ~85% worker idle).
//
// inflight guards against re-dispatching a domain that is still being crawled/stamped: selectTargets
// keys on web_crawled_at IS NULL and returns the same head each call until those rows are stamped,
// so without dedup the producer would re-feed in-flight domains. Entries are freed only AFTER a
// domain is stamped (flushed) — or immediately if it was deferred (never crawled). The buffered
// jobs channel provides backpressure so we never select faster than workers consume. Runs until ctx ends; then the
// producer stops, the workers finish what they hold, and the consumer writes the last results and returns.
func crawlContinuous(ctx context.Context, db *sql.DB, spool *dbresil.Spooler, workers, dbBatch int, upsert, delStmt, emitStmt, stampStmt, touchStmt *sql.Stmt) {
	jobs := make(chan target, workers*2)
	results := make(chan result, workers*4)
	var inflight sync.Map // domain -> struct{}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				results <- safeCrawl(t)
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()

	// crawlBatch prunes these per batch; with no batches, prune on a timer so they stay bounded
	// (robotsCache growing unbounded was the 2026-07-13 memory leak).
	go func() {
		t := time.NewTicker(2 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			// Prune IN PLACE — the workers hold &hostGates etc. and call Load/Store concurrently;
			// reassigning the struct (=sync.Map{}) raced their internal state. Gates are pruned rather than cleared,
			// so a gap still in effect (up to the 30 s Crawl-delay cap) is never cut short.
			pruneGates(&hostGates, gateIdle)
			pruneGates(&ipGates, gateIdle)
			robotsCache.Clear()
			// sitemapChecked never shrinks in PIPELINE mode (no batches), so
			// prune it on the timer too; entries older than the recheck
			// interval can never suppress a fetch again.
			pruneSitemapChecked()
			breaker.prune(2 * breaker.cap)
			// Return idle heap to the OS (2026-08-25). The memstats instrumentation proved the RSS
			// growth is NOT a code leak — heapInuse/heapObjects/goroutines oscillate and return to
			// baseline, but heapSys ratchets up (~20MB/hr) because the runtime keeps the arenas grown
			// by burst allocations (24 concurrent html.Parse) reserved up toward GOMEMLIMIT instead of
			// scavenging them between bursts. FreeOSMemory forces an immediate GC + return, so RSS
			// tracks the ~80-100MB working set instead of ratcheting to 570-690MB over days. This is
			// the durable fix for the fragmentation. Cost: a forced GC every 2 min.
			debug.FreeOSMemory()
		}
	}()

	// Producer: keep the jobs channel fed, skipping domains already in the pipeline. Closes jobs when ctx ends.
	go func() {
		defer close(jobs)
		for ctx.Err() == nil {
			crawlSubdomains.Store(crawlutil.LoadCrawlSubdomains(db, crawlSubdomains.Load()))
			targets, err := selectTargets(db)
			if err != nil {
				log.Printf("select failed (%v) — waiting for remote DB", err)
				if dbresil.WaitForRemoteCtx(ctx, db) != nil {
					return
				}
				continue
			}
			// Feed workers in OPIC score order from the frontier (the priority);
			// shuffling here used to neutralize T2 OPIC priority.
			fed := 0
			for _, t := range targets {
				if _, dup := inflight.LoadOrStore(t.domain, struct{}{}); dup {
					continue // already crawling/awaiting-stamp
				}
				select {
				case jobs <- t: // backpressure: blocks when workers are saturated
					fed++
				case <-ctx.Done():
					inflight.Delete(t.domain)
					return
				}
			}
			if fed == 0 { // all selected rows are already in-flight (or no work) → let stamps catch up
				dbresil.SleepCtx(ctx, 15*time.Second)
			} else if fed < 20 {
				// 2026-09-28: a trickle means the crawlable frontier is (nearly) drained — new rows only arrive with the
				// next snapshot reload / recrawl window — so back off instead of re-selecting in a tight loop.
				dbresil.SleepCtx(ctx, 5*time.Second)
			}
		}
	}()

	// Consumer: process results, batch DB writes (dbBatch rows OR every few seconds), and free
	// each domain from inflight only after it's stamped, so it can't be re-selected mid-flight.
	batch := make([]crawlOut, 0, dbBatch)
	pending := make([]string, 0, dbBatch)
	var nOK, fail, skip, blk, chal, defr, disc, unch int64
	flush := func() {
		if len(batch) > 0 {
			disc += int64(flushBatch(db, spool, batch, upsert, delStmt, stampStmt, emitStmt, touchStmt))
			batch = batch[:0]
		}
		for _, d := range pending {
			inflight.Delete(d)
		}
		pending = pending[:0]
	}
	flushTick := time.NewTicker(5 * time.Second)
	defer flushTick.Stop()
	logTick := time.NewTicker(30 * time.Second)
	defer logTick.Stop()
	log.Printf("crawl mode: PIPELINE (continuous, no batch barrier) workers=%d", workers)
	for {
		select {
		case r, open := <-results:
			if !open { // shutting down and every worker has finished
				flush()
				log.Printf("pipeline stopped: ok=%d unchanged=%d fail=%d skip=%d blocked=%d challenge=%d deferred=%d new=%d", nOK, unch, fail, skip, blk, chal, defr, disc)
				return
			}
			if r.deferred {
				defr++
				inflight.Delete(r.domain) // not crawled → free to retry when the breaker closes
				continue
			}
			out, oc, _ := classify(r, emitStmt != nil)
			switch oc {
			case outSkipped:
				skip++
			case outFailed:
				fail++
			case outUnchanged:
				unch++ // 304 → bump crawled_at only, keep the stored row
			case outChallenged:
				chal++
			default:
				nOK++
			}
			if out.bad {
				blk++
			}
			batch = append(batch, out)
			pending = append(pending, r.domain)
			if len(batch) >= dbBatch {
				flush()
			}
		case <-flushTick.C:
			flush() // stamp partial batches promptly so their domains leave the frontier
		case <-logTick.C:
			log.Printf("pipeline: ok=%d unchanged=%d fail=%d skip=%d blocked=%d challenge=%d deferred=%d new=%d", nOK, unch, fail, skip, blk, chal, defr, disc)
			// Leak instrumentation (2026-08-25): RSS climbs past GOMEMLIMIT to ~700MB/5-6 days but the
			// known maps are pruned every 2 min and threads stay low — so log the Go runtime's own
			// view to pinpoint the accumulator. Which metric CLIMBS over hours identifies the class:
			//   heapObjects/heapInuse up → a reachable-object heap leak; goroutines/stackInuse up → a
			//   goroutine leak; heapSys≫heapInuse (idle up) → scavenger/fragmentation (not a true leak).
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			log.Printf("memstats: goroutines=%d heapInuse=%dMB heapObjects=%d heapSys=%dMB heapIdle=%dMB heapReleased=%dMB stackInuse=%dMB nextGC=%dMB numGC=%d",
				runtime.NumGoroutine(), ms.HeapInuse>>20, ms.HeapObjects, ms.HeapSys>>20, ms.HeapIdle>>20, ms.HeapReleased>>20, ms.StackInuse>>20, ms.NextGC>>20, ms.NumGC)
		}
	}
}

// outcome is how one crawl result is counted.
type outcome int

const (
	outOK outcome = iota
	outUnchanged
	outFailed
	outSkipped
	outChallenged
)

// classify turns one crawl result into the row to write and how to count it; both crawl loops use it. blockedBy is the
// blacklist entry that matched ("" if none) — a blacklisted row is deleted rather than written, and the blacklist
// beats a 304 touch.
func classify(r result, emitEnabled bool) (out crawlOut, oc outcome, blockedBy string) {
	var info WebsiteInfo
	var ferr interface{}
	notmod := false
	switch {
	case r.note != "":
		ferr, oc = r.note, outSkipped
	case r.err != nil:
		ferr, oc = r.err.Error(), outFailed
	default:
		info = *r.info
		switch {
		case info.NotModified:
			notmod, oc = true, outUnchanged // conditional GET: the stored copy is current, only bump crawled_at
		case info.Challenge != "":
			// A bot-challenge interstitial: keep the status, record the vendor in fetch_error; its content was
			// blanked at fetch time, so nothing from it can be indexed.
			ferr, oc = "challenge:"+info.Challenge, outChallenged
		default:
			oc = outOK
		}
	}
	term, bad := isBlacklisted(r.domain, &info)
	if bad {
		notmod, blockedBy = false, term
	}
	// Discovery emit only from successfully crawled, non-blacklisted, non-challenge pages that found external
	// domains (a 304 carries none).
	emit := emitEnabled && !bad && r.err == nil && r.note == "" && r.info != nil && r.info.Challenge == "" && len(r.info.outDomains) > 0
	return crawlOut{domain: r.domain, info: info, ferr: ferr, bad: bad, emit: emit, notmod: notmod}, oc, blockedBy
}

// crawlOut is a buffered crawl result awaiting a batched DB write.
type crawlOut struct {
	domain string
	info   WebsiteInfo
	ferr   interface{} // fetch_error column value (string or nil)
	bad    bool        // matched the blacklist → delete instead of upsert
	emit   bool        // eligible to emit discovered external domains
	notmod bool        // 304 Not Modified → touch crawled_at only, don't rewrite the row
}

const upsertHead = `INSERT INTO website_info (domain,title,description,keywords,canonical,h1,status_code,content_type,content_length,latency_ms,redirects,outbound_links,image_count,word_count,charset,robots,og_title,og_image,twitter_card,page_language,server_header,fetch_error,etag,last_modified,simhash,simhash_b0,simhash_b1,simhash_b2,simhash_b3,duplicate_of,crawled_at) VALUES `

// IG2: findDuplicate checks if a SimHash fingerprint is a near-duplicate of an
// existing page. Uses the block-index technique: near-dupes (distance <= 3)
// share at least one 16-bit block. Returns the duplicate domain, or "".
// IG3: fetchSitemap tries to fetch and parse /sitemap.xml for a domain.
// Returns URL count and latest lastmod. Returns (0, zero) on any failure.
// Opt-in via SITEMAP_FETCH (doubles requests when enabled).
func fetchSitemap(ctx context.Context, domain string) (int, time.Time) {
	if !sitemapFetch {
		return 0, time.Time{}
	}
	// P1 SSRF fix: use the guarded client (netguard.Control via transport)
	// instead of a bare http.Client. A sitemap URL or redirect to
	// 169.254.169.254 / 10.x / 127.0.0.1 must be refused at dial time.
	client := newClient()
	// Try https first, then http.
	for _, scheme := range []string{"https", "http"} {
		sitemapURL := scheme + "://" + domain + "/sitemap.xml"
		req, err := http.NewRequestWithContext(ctx, "GET", sitemapURL, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			continue
		}
		urls, sitemapURLs, isIndex, err := sitemap.Parse(bytes.NewReader(body))
		if err != nil {
			continue
		}
		if isIndex && len(sitemapURLs) > 0 {
			// Fetch the first child sitemap (bounded).
			return fetchSitemapURL(ctx, sitemapURLs[0])
		}
		if len(urls) > 0 {
			minc(mSitemapFound)
			return len(urls), sitemap.LatestLastMod(urls)
		}
	}
	return 0, time.Time{}
}

// fetchSitemapURL fetches and parses a single sitemap URL.
func fetchSitemapURL(ctx context.Context, sitemapURL string) (int, time.Time) {
	// P1 SSRF fix: guarded client (see fetchSitemap).
	client := newClient()
	req, err := http.NewRequestWithContext(ctx, "GET", sitemapURL, nil)
	if err != nil {
		return 0, time.Time{}
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return 0, time.Time{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, time.Time{}
	}
	urls, _, _, err := sitemap.Parse(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return 0, time.Time{}
	}
	if len(urls) > 0 {
		minc(mSitemapFound)
	}
	return len(urls), sitemap.LatestLastMod(urls)
}

func findDuplicate(ctx context.Context, db *sql.DB, fp uint64) string {
	if fp == 0 {
		return ""
	}
	blocks := simhash.Blocks(fp)
	// Query candidates matching any block (indexed).
	rows, err := db.QueryContext(ctx, `
		SELECT domain, simhash FROM website_info
		WHERE simhash_b0 = $1 OR simhash_b1 = $2 OR simhash_b2 = $3 OR simhash_b3 = $4
		LIMIT 100`,
		int16(blocks[0]), int16(blocks[1]), int16(blocks[2]), int16(blocks[3]))
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var domain string
		var stored int64
		if err := rows.Scan(&domain, &stored); err != nil {
			continue
		}
		if simhash.IsDuplicate(fp, uint64(stored)) {
			return domain
		}
	}
	return ""
}

const upsertTail = ` ON CONFLICT (domain) DO UPDATE SET
	title=EXCLUDED.title, description=EXCLUDED.description, keywords=EXCLUDED.keywords,
	canonical=EXCLUDED.canonical, h1=EXCLUDED.h1, status_code=EXCLUDED.status_code,
	content_type=EXCLUDED.content_type, content_length=EXCLUDED.content_length,
	latency_ms=EXCLUDED.latency_ms, redirects=EXCLUDED.redirects,
	outbound_links=EXCLUDED.outbound_links, image_count=EXCLUDED.image_count,
	word_count=EXCLUDED.word_count, charset=EXCLUDED.charset, robots=EXCLUDED.robots,
	og_title=EXCLUDED.og_title, og_image=EXCLUDED.og_image, twitter_card=EXCLUDED.twitter_card,
	page_language=EXCLUDED.page_language, server_header=EXCLUDED.server_header,
	fetch_error=EXCLUDED.fetch_error, etag=EXCLUDED.etag, last_modified=EXCLUDED.last_modified,
	crawled_at=now(),
	-- This crawler re-authored the row, so it now owns it. Reset provenance (NULL); otherwise a row
	-- first created by node 'x' keeps source_node='x' forever and a "purge a bad node"
	-- DELETE WHERE source_node='x' would nuke rows this crawler now owns.
	source_node=NULL`

// batchWriteTimeout bounds a single batch write (Begin+Exec+Commit) as a unit — see flushBatch.
// Generous: a healthy batch commits in <1s; this only trips on a genuinely wedged connection (which
// statement_timeout, bounding query execution only, does not cover). A trip → row-by-row fallback.
const batchWriteTimeout = 120 * time.Second

// flushBatch persists a batch of crawl results. It tries ONE transaction (multi-row upsert +
// a single delete/stamp/emit); on any error it rolls back and falls back to the original
// row-by-row path, so a single poison row can't drop the whole batch. Returns the number of
// newly-discovered domains inserted.
func flushBatch(remote *sql.DB, spool *dbresil.Spooler, batch []crawlOut, upsert, delStmt, stampStmt, emitStmt, touchStmt *sql.Stmt) int {
	if len(batch) == 0 {
		return 0
	}
	// NR3: at the pages cap, only update rows that already exist — new
	// domains are skipped (they stay unstamped and are retried when there's
	// room). Updates don't grow the table.
	if fpChecker != nil {
		if pause, reason := fpChecker.PauseNewPages(); pause {
			batch = filterExistingDomains(remote, batch)
			log.Printf("[INFO] %s — %d existing-domain update(s) kept", reason, len(batch))
			if len(batch) == 0 {
				return 0
			}
		}
	}
	// Bound the whole batch write with its own deadline, derived from Background
	// (NOT a shutdown ctx) — so a wedged connect/commit is cut off while a graceful restart still lets
	// an in-flight batch COMMIT rather than discarding completed crawl work. Same ctx for the spool path.
	ctx, cancel := context.WithTimeout(context.Background(), batchWriteTimeout)
	defer cancel()
	n, err := flushBatchTx(ctx, remote, batch, emitStmt != nil, false)
	if err == nil {
		return n
	}
	// Remote write failed. If the remote is DOWN, spool to the local outbox when one is configured (skipStamp: local
	// domain_info has no web_crawled_at); without one the batch is dropped — its domains stay unstamped and are
	// crawled again later — instead of failing row by row against a dead database. Otherwise it's a data error →
	// isolate it row-by-row against the remote.
	if dbresil.IsDown(remote, err) {
		if spool == nil {
			log.Printf("database down — %d result(s) not written; their domains stay unstamped and will be crawled again", len(batch))
			return 0
		}
		if spool.OverCap("website_info") {
			log.Printf("spool over capacity — dropping batch of %d (remote down)", len(batch))
			return 0
		}
		sn, serr := flushBatchTx(ctx, spool.DB(), batch, emitStmt != nil, true)
		if serr != nil {
			log.Printf("spool write failed (%v) — dropping batch of %d", serr, len(batch))
			return 0
		}
		log.Printf("remote down — spooled %d row(s) to local outbox", len(batch))
		return sn
	}
	log.Printf("batch flush failed (%v) — falling back to row-by-row for %d row(s)", err, len(batch))
	return flushRowByRow(batch, upsert, delStmt, stampStmt, emitStmt, touchStmt)
}

// minc safely increments a metrics counter (nil when metrics aren't initialized).
func minc(c *atomic.Int64) {
	if c != nil {
		c.Add(1)
	}
}

// filterExistingDomains keeps only batch entries whose domain is already in
// website_info (NR3: at the pages cap, updates are fine, inserts are not).
func filterExistingDomains(db *sql.DB, batch []crawlOut) []crawlOut {
	domains := make([]string, 0, len(batch))
	for _, b := range batch {
		domains = append(domains, b.domain)
	}
	rows, err := db.Query(`SELECT domain FROM website_info WHERE domain = ANY($1)`, pq.Array(domains))
	if err != nil {
		log.Printf("footprint: existing-domain check failed (%v) — keeping full batch", err)
		return batch
	}
	defer rows.Close()
	exists := map[string]bool{}
	for rows.Next() {
		var d string
		rows.Scan(&d)
		exists[d] = true
	}
	out := batch[:0]
	for _, b := range batch {
		if exists[b.domain] {
			out = append(out, b)
		}
	}
	return out
}

func flushBatchTx(ctx context.Context, db *sql.DB, batch []crawlOut, emitEnabled bool, skipStamp bool) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // no-op after a successful Commit

	good := make([]crawlOut, 0, len(batch))
	var bad, notmod, all []string
	for _, b := range batch {
		all = append(all, b.domain)
		switch {
		case b.bad:
			bad = append(bad, b.domain)
		case b.notmod:
			notmod = append(notmod, b.domain)
		default:
			// IG2: check for near-duplicates before storing.
			if b.info.SimHash != 0 {
				if dup := findDuplicate(ctx, db, b.info.SimHash); dup != "" && dup != b.domain {
					b.info.DuplicateOf = dup
					minc(mCrawledDuplicates)
				}
			}
			good = append(good, b)
		}
	}

	// Deadlock avoidance: emit rows in SORTED domain order so this writer and any other writer that sorts the
	// same way acquire website_info row locks in the SAME order over an overlapping domain set and can't deadlock.
	// Cheap: batches are dbBatch-sized. Sorting bad/notmod/all also fixes the ANY()-array DELETE/UPDATE
	// lock order below. A deadlock here aborts the tx → row-by-row fallback → throughput loss, so this
	// matters even though it's rare.
	sort.Slice(good, func(i, j int) bool { return good[i].domain < good[j].domain })
	sort.Strings(bad)
	sort.Strings(notmod)
	sort.Strings(all)

	// 1) multi-row upsert (ON CONFLICT refresh) for everything not blacklisted.
	if len(good) > 0 {
		const nc = 30
		var sb strings.Builder
		sb.WriteString(upsertHead)
		args := make([]interface{}, 0, len(good)*nc)
		for i, b := range good {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteByte('(')
			for j := 0; j < nc; j++ {
				if j > 0 {
					sb.WriteByte(',')
				}
				sb.WriteByte('$')
				sb.WriteString(strconv.Itoa(i*nc + j + 1))
			}
			sb.WriteString(",now())")
			in := b.info
			args = append(args,
				b.domain, ns(in.Title), ns(in.Description), ns(in.Keywords), ns(in.Canonical), ns(in.H1),
				in.StatusCode, ns(in.ContentType), in.ContentLength, in.LatencyMs, in.RedirectCount,
				in.OutboundLinkCount, in.ImageCount, in.WordCount, ns(in.Charset), ns(in.Robots),
				ns(in.OGTitle), ns(in.OGImage), ns(in.TwitterCard), ns(in.PageLanguage), ns(in.ServerHeader),
				b.ferr, ns(in.ETag), ns(in.LastModified),
				int64(in.SimHash), int16(simhash.Blocks(in.SimHash)[0]), int16(simhash.Blocks(in.SimHash)[1]),
				int16(simhash.Blocks(in.SimHash)[2]), int16(simhash.Blocks(in.SimHash)[3]), ns(in.DuplicateOf))
		}
		sb.WriteString(upsertTail)
		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return 0, err
		}
	}

	// 2) blacklisted → remove any existing row.
	if len(bad) > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM website_info WHERE domain = ANY($1)`, pq.Array(bad)); err != nil {
			return 0, err
		}
	}

	// 2b) 304 Not Modified → content unchanged; refresh crawled_at only, leave the row intact.
	//     (On the local spool DB the row usually isn't present → a harmless 0-row update.)
	if len(notmod) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE website_info SET crawled_at = now() WHERE domain = ANY($1)`, pq.Array(notmod)); err != nil {
			return 0, err
		}
	}

	// 3) stamp EVERY attempted domain (crawl-state marker). Skipped when spooling to the local
	//    outbox, whose domain_info has no web_crawled_at and where stamps aren't worth replaying.
	//    T3: adaptive recrawl interval — 304s (unchanged) back off ×1.25 (cap 30d);
	//    successful 200s (changed) shorten ×0.75 (floor 1d). Failures keep the
	//    current interval. NULL interval means "global RECRAWL_DAYS".
	if !skipStamp {
		defInterval := float64(recrawlDays())
		if len(notmod) > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE domain_info
				SET web_crawled_at = now(),
				    recrawl_interval_days = LEAST(COALESCE(recrawl_interval_days, $2) * 1.25, 30)
				WHERE domain = ANY($1)`, pq.Array(notmod), defInterval); err != nil {
				return 0, err
			}
		}
		goodDomains := make([]string, 0, len(good))
		for _, b := range good {
			goodDomains = append(goodDomains, b.domain)
		}
		if len(goodDomains) > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE domain_info
				SET web_crawled_at = now(),
				    recrawl_interval_days = GREATEST(COALESCE(recrawl_interval_days, $2) * 0.75, 1)
				WHERE domain = ANY($1)`, pq.Array(goodDomains), defInterval); err != nil {
				return 0, err
			}
		}
		// Domains neither 304 nor 200 (failures, etc.): stamp only, keep interval.
		rest := make([]string, 0, len(all)-len(notmod)-len(goodDomains))
		inNotmod := make(map[string]bool, len(notmod))
		for _, d := range notmod {
			inNotmod[d] = true
		}
		inGood := make(map[string]bool, len(goodDomains))
		for _, d := range goodDomains {
			inGood[d] = true
		}
		for _, d := range all {
			if !inNotmod[d] && !inGood[d] {
				rest = append(rest, d)
			}
		}
		if len(rest) > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE domain_info SET web_crawled_at = now() WHERE domain = ANY($1)`, pq.Array(rest)); err != nil {
				return 0, err
			}
		}
	}

	// 4) discovery emit: dedup within the batch (a domain found on several pages → one insert),
	//    keep the per-page 150 cap, and let NOT EXISTS dedup against existing rows.
	discovered := 0
	if emitEnabled {
		type disc struct{ d, f string }
		var pairs []disc
		seen := make(map[string]bool)
		for _, b := range good {
			if !b.emit {
				continue
			}
			from := "https://" + b.domain + "/"
			cnt := 0
			for _, d := range sortedKeys(b.info.outDomains) { // sorted, so the per-page cap keeps the same 150 every time
				if seen[d] || nameBlacklisted(d) {
					continue
				}
				seen[d] = true
				pairs = append(pairs, disc{d, from})
				if cnt++; cnt >= 150 {
					break
				}
			}
		}
		// Deadlock avoidance (2026-08-14): the discovery INSERT was the last cross-writer deadlock source
		// on domain_info — its domains came from map iteration (UNORDERED) and raced the other domain_info
		// writers. Sort by domain so it takes row/unique-index locks in a deterministic order, matching the
		// sorted UPDATE above. (Full elimination also needs the other writers' inserts sorted; this
		// removes the crawler's own contribution — measure the deadlock rate before doing the siblings.)
		sort.Slice(pairs, func(i, j int) bool { return pairs[i].d < pairs[j].d })
		if len(pairs) > 0 {
			ed := make([]string, len(pairs))
			ef := make([]string, len(pairs))
			for i, p := range pairs {
				ed[i], ef[i] = p.d, p.f
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO domain_info (domain, found_on, crawled_at)
				SELECT d, f, now() FROM unnest($1::text[], $2::text[]) AS t(d, f)
				WHERE NOT EXISTS (SELECT 1 FROM domain_info di WHERE di.domain = t.d)`,
				pq.Array(ed), pq.Array(ef))
			if err != nil {
				return 0, err
			}
			if ra, e := res.RowsAffected(); e == nil {
				discovered = int(ra)
			}
		}
	}

	// T2: OPIC cash distribution. For each successfully crawled domain, spread
	// its cash across its outlink domains, then reset its cash to zero and
	// accumulate history. Runs in the same transaction as the discovery emit.
	{
		sources := make(map[string][]string)
		for _, b := range batch {
			if b.info.outDomains == nil || len(b.info.outDomains) == 0 {
				continue
			}
			// Only distribute from successfully crawled pages (not 304s, which
			// carry no outlink information).
			if b.notmod || b.bad || b.ferr != nil {
				continue
			}
			outs := make([]string, 0, len(b.info.outDomains))
			for d := range b.info.outDomains {
				outs = append(outs, d)
			}
			sort.Strings(outs)
			sources[b.domain] = outs
		}
		if err := opicDistribute(ctx, tx, sources); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return discovered, nil
}

// flushRowByRow is the resilient fallback: the original per-row path using the prepared
// statements, used only when a batched transaction fails.
func flushRowByRow(batch []crawlOut, upsert, delStmt, stampStmt, emitStmt, touchStmt *sql.Stmt) int {
	discovered := 0
	for _, b := range batch {
		switch {
		case b.bad:
			if _, err := delStmt.Exec(b.domain); err != nil {
				log.Printf("blacklist delete %s: %v", b.domain, err)
			}
		case b.notmod:
			// 304 → content unchanged; bump crawled_at without touching the stored row.
			if _, err := touchStmt.Exec(b.domain); err != nil {
				log.Printf("touch %s: %v", b.domain, err)
			}
		default:
			in := b.info
			if _, err := upsert.Exec(
				b.domain, ns(in.Title), ns(in.Description), ns(in.Keywords), ns(in.Canonical), ns(in.H1),
				in.StatusCode, ns(in.ContentType), in.ContentLength, in.LatencyMs, in.RedirectCount,
				in.OutboundLinkCount, in.ImageCount, in.WordCount, ns(in.Charset), ns(in.Robots),
				ns(in.OGTitle), ns(in.OGImage), ns(in.TwitterCard), ns(in.PageLanguage), ns(in.ServerHeader),
				b.ferr, ns(in.ETag), ns(in.LastModified),
				int64(in.SimHash), int16(simhash.Blocks(in.SimHash)[0]), int16(simhash.Blocks(in.SimHash)[1]),
				int16(simhash.Blocks(in.SimHash)[2]), int16(simhash.Blocks(in.SimHash)[3]), ns(in.DuplicateOf),
			); err != nil {
				// A failed upsert stored NO content — do NOT fall through to the web_crawled_at
				// stamp below, or this row is hidden from re-crawl for RECRAWL_DAYS. Leave it
				// unstamped so the next cycle retries it.
				log.Printf("upsert %s: %v — leaving uncrawled to retry", b.domain, err)
				continue
			}
		}
		if emitStmt != nil && b.emit {
			discovered += emitDomains(emitStmt, b.domain, b.info.outDomains)
		}
		if _, err := stampStmt.Exec(b.domain); err != nil {
			log.Printf("stamp %s: %v", b.domain, err)
		}
	}
	return discovered
}

// ns sanitizes to valid UTF-8 (Postgres rejects invalid byte sequences) as a
// safety net even after charset transcoding, and converts ""→NULL.
func ns(s string) interface{} {
	s = strings.ToValidUTF8(s, "")
	s = strings.ReplaceAll(s, "\x00", "") // Postgres TEXT rejects NUL bytes (0x00) even though they're valid UTF-8
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// nullableTime returns nil for zero time, else the time (for SQL NULL).
func nullableTime(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t
}

// emitDomains inserts the external domains found on a crawled page into domain_info
// (skipping ones already present), reusing the fetch the crawler already did.
// Returns how many NEW domains were inserted. Capped per page to bound the array.
// nameBlacklisted reports whether a domain NAME contains a banned term — used to
// avoid seeding obviously-bad domains into the crawl queue at discovery time.
func nameBlacklisted(domain string) bool {
	blMu.RLock()
	defer blMu.RUnlock()
	if blDomainHit(domain) {
		return true
	}
	d := strings.ToLower(domain)
	for _, t := range blWords {
		if blWordBoundary(d, t) {
			return true
		}
	}
	return false
}

// sortedKeys returns m's keys in order, so caps over a map pick the same entries every run.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func emitDomains(stmt *sql.Stmt, fromDomain string, set map[string]bool) int {
	doms := make([]string, 0, len(set))
	for _, d := range sortedKeys(set) {
		if nameBlacklisted(d) {
			continue // don't queue domains whose name already trips the blacklist
		}
		doms = append(doms, d)
		if len(doms) >= 150 {
			break
		}
	}
	res, err := stmt.Exec("https://"+fromDomain+"/", pq.Array(doms))
	if err != nil {
		log.Printf("emit from %s: %v", fromDomain, err)
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

// clip truncates a string to n runes for tidy log lines.
// Bounded: walks at most n+1 runes, so a multi-MB input (keyword-stuffed
// pages have hit 4.5MB) costs ~n bytes of work, not 4x its full length the
// way []rune(s) did.
func clip(s string, n int) string {
	if len(s) <= n {
		return s // ASCII fast path: n runes need at least n bytes
	}
	i, runes := 0, 0
	for i < len(s) && runes < n {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		runes++
	}
	if i >= len(s) {
		return s
	}
	return s[:i] + "…"
}

const (
	maxTitleLen    = 200
	maxDescLen     = 1000 // meta/og description (also feeds snippets)
	maxKeywordsLen = 500  // meta keywords list (some pages stuffed 4.5MB here)
	maxURLLen      = 2048 // canonical / og:image; a longer value is dropped, not cut (a cut URL is useless)
	maxShortLen    = 200  // robots meta, Server header, Content-Type
	maxTagLen      = 40   // charset, twitter:card, page language
)

// dropIfLong returns s, or "" when it is longer than n bytes (for URLs and HTTP validators, which must be exact).
func dropIfLong(s string, n int) string {
	if len(s) > n {
		return ""
	}
	return s
}

// cleanTitle defends against RCDATA absorption: an unclosed or typo'd </title>
// (e.g. </Tittle>, <</title>) makes the HTML parser swallow the rest of the page
// as title text. Real title text has no raw '<', so cut there, then hard-cap length.
func cleanTitle(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '<'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return clip(s, maxTitleLen)
}

// safeCrawl runs crawlOne but recovers from panics (e.g. the html parser on
// malformed input) so one bad page can never crash the whole crawler.
func safeCrawl(t target) (r result) {
	defer func() {
		if rec := recover(); rec != nil {
			// Dump the goroutine stack so the panic SITE (not just the message) is
			// identifiable in the log; the per-domain err still routes to fetch_error.
			log.Printf("PANIC crawling %s: %v\n%s", t.domain, rec, debug.Stack())
			r = result{domain: t.domain, err: fmt.Errorf("panic: %v", rec)}
		}
	}()
	return crawlOne(t)
}

// crawlOne crawls a single domain. A failed request is not retried in the same cycle (a dropped, reset or
// timed-out connection is the server refusing us). A 429/503 is retried once, after its Retry-After, but only when
// that is at most throttleWaitMax; a longer one is respected by leaving the site alone until a later cycle.
// Timeout/throttle outcomes feed the per-ASN/IP breaker; when a group's breaker is open the domain is deferred
// (skipped, left uncrawled) rather than hammered again.
func crawlOne(t target) result {
	domain := t.domain
	key := t.groupKey()
	if breaker.blocked(key) {
		return result{domain: domain, deferred: true} // group in cooldown → skip, retry a later cycle
	}
	// Every request is paced per server IP, so a target selected before dns-enricher reached it is resolved here.
	// A name that only resolves to non-public addresses is never contacted (netguard also refuses at connect time).
	if t.ip == "" {
		ip, err := lookupIP(domain)
		if err != nil {
			return result{domain: domain, err: err}
		}
		t.ip = ip
	}
	if !netguard.PublicIP(t.ip) {
		return result{domain: domain, note: "blocked:non-public-address"}
	}
	hostThrottle(domain)          // per-site spacing; also paces the robots.txt fetch below
	ri := robotsFor(domain, t.ip) // robots.txt is always read first; its GET passes the server-IP gate too
	if ri.blockErr != nil {
		// The server dropped/reset/timed out our robots.txt request — it is refusing us. Don't follow up
		// with the page GET (a second, unpaced hit per domain is exactly what abuse reports describe);
		// count it against the group so its siblings back off too.
		breaker.fail(key)
		return result{domain: domain, err: ri.blockErr}
	}
	if !ri.allowed {
		return result{domain: domain, note: "robots_disallow"}
	}
	// The page GET is paced against the robots.txt GET on both gates: the server IP (shared hosting → many names,
	// one box) and the site, using the site's Crawl-delay when it asks for more than our default. Stamping the site
	// gate here (not only before robots.txt) is what spaces any redirect hop from the page GET itself.
	ipThrottle(t.ip)
	sitePace(domain, ri.delay)
	throttleBudget := throttleRetries
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		info, err := fetchWebsiteInfo(ctx, domain, t.ip, t.etag, t.lastModified, ri.rules)
		cancel()
		if err != nil {
			if isBackoffError(err) {
				breaker.fail(key) // timeout / connection failure counts against this group
			}
			return result{domain: domain, err: err}
		}
		if retryThrottled(info, throttleBudget) {
			throttleBudget--
			wait := info.RetryAfter
			if wait <= 0 {
				wait = time.Duration(attempt+1) * time.Second // no Retry-After header: linear backoff
			}
			time.Sleep(wait)
			ipThrottle(t.ip) // the retry is a request like any other
			sitePace(domain, ri.delay)
			continue
		}
		// Final outcome: a 429/503 we couldn't (or shouldn't) wait out is a throttle signal (count it);
		// any other real HTTP response means the infra is healthy → clear the group's failures.
		if throttled(info) {
			breaker.fail(key)
		} else {
			breaker.success(key)
		}
		// T1: sitemap-driven discovery. Sitemaps list URLs the homepage crawl
		// would never find; discovered domains join the normal emit path.
		sitemapDiscover(domain, t.ip, ri, info)
		return result{domain: domain, info: info}
	}
}

// errRobotsDisallowed is returned by crawlOneURL when robots.txt disallows the crawl.
var errRobotsDisallowed = errors.New("robots.txt disallows /")

// crawlOneURL is the testable single-URL crawl entrypoint (H1 e2e). It runs the
// real pipeline for one URL: robots.txt verdict via robotsFor, fetch+parse via
// fetchWebsiteInfo through the netguard-guarded transport (including the
// 429/503 retry), and persist via flushBatchTx. Production crawls use
// crawlOne(target), which layers DNS/frontier/throttle/breaker concerns on top;
// this entrypoint skips those so tests can drive the pipeline against fake
// origins directly.
func crawlOneURL(ctx context.Context, db *sql.DB, rawURL string) (*WebsiteInfo, error) {
	info, host, err := fetchOneURLInfo(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	// IG3: opt-in sitemap fetch for recrawl signals.
	if sitemapFetch && info.StatusCode == 200 {
		if n, lm := fetchSitemap(ctx, host); n > 0 {
			info.SitemapURLs = n
			info.SitemapLastMod = lm
		}
	}
	batch := []crawlOut{{domain: host, info: *info}}
	if _, err := flushBatchTx(ctx, db, batch, false, false); err != nil {
		return nil, fmt.Errorf("persist: %w", err)
	}
	return info, nil
}

// fetchOneURLInfo runs one URL through the real pipeline — robots check,
// fetch, parse — but writes nothing. It returns the fetched info and the
// host. Used by crawlOneURL (which then persists) and by -single-url
// dry-run mode (which prints the envelope instead).
func fetchOneURLInfo(ctx context.Context, rawURL string) (*WebsiteInfo, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("parse url: %w", err)
	}
	host := u.Host
	if host == "" {
		return nil, "", fmt.Errorf("url %q has no host", rawURL)
	}
	ri := robotsFor(host, "")
	if ri.blockErr != nil {
		return nil, "", ri.blockErr
	}
	if !ri.allowed {
		return nil, "", errRobotsDisallowed
	}
	throttleBudget := throttleRetries
	for {
		info, err := fetchWebsiteInfo(ctx, host, "", "", "", ri.rules)
		if err != nil {
			return nil, "", err
		}
		if info == nil {
			return nil, "", fmt.Errorf("fetchWebsiteInfo returned nil info, nil error")
		}
		if retryThrottled(info, throttleBudget) {
			throttleBudget--
			wait := info.RetryAfter
			if wait <= 0 {
				wait = time.Second
			}
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		return info, host, nil
	}
}

// dryRunEnvelope renders a WebsiteInfo as the JSON row cl0q-node would send
// to the ingest API (same field names as wiRow), so -single-url prints
// exactly what a real crawl of the URL would submit.
func dryRunEnvelope(host string, info *WebsiteInfo) map[string]any {
	fetchErr := ""
	if info.Challenge != "" {
		fetchErr = "challenge:" + info.Challenge
	}
	return map[string]any{
		"domain":         host,
		"title":          info.Title,
		"description":    info.Description,
		"keywords":       info.Keywords,
		"canonical":      info.Canonical,
		"h1":             info.H1,
		"status_code":    info.StatusCode,
		"content_type":   info.ContentType,
		"content_length": info.ContentLength,
		"latency_ms":     info.LatencyMs,
		"redirects":      info.RedirectCount,
		"outbound_links": info.OutboundLinkCount,
		"image_count":    info.ImageCount,
		"word_count":     info.WordCount,
		"charset":        info.Charset,
		"robots":         info.Robots,
		"og_title":       info.OGTitle,
		"og_image":       info.OGImage,
		"twitter_card":   info.TwitterCard,
		"page_language":  info.PageLanguage,
		"server_header":  info.ServerHeader,
		"fetch_error":    fetchErr,
		"etag":           info.ETag,
		"last_modified":  info.LastModified,
	}
}

// sitePace waits out the per-site gap before a request to host: hostDelay, or the site's Crawl-delay if longer.
func sitePace(host string, crawlDelay time.Duration) {
	throttle(&hostGates, hostKey(strings.ToLower(host)), max(hostDelay, crawlDelay))
}

// throttled reports a 429 Too Many Requests / 503 Service Unavailable response.
func throttled(info *WebsiteInfo) bool {
	return info.StatusCode == http.StatusTooManyRequests || info.StatusCode == http.StatusServiceUnavailable
}

// retryThrottled decides whether a throttled response gets its single retry: only while the retry budget lasts and
// only if the server's Retry-After (0 = none given) is no longer than we are willing to wait.
func retryThrottled(info *WebsiteInfo, budget int) bool {
	return throttled(info) && budget > 0 && info.RetryAfter <= throttleWaitMax
}

// lookupIP resolves host to the address its requests are paced by: the first public IPv4, else the first public
// IPv6, else (all-private) the first address, which crawlOne then refuses. Answers come from the DNS cache.
func lookupIP(host string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ipaddrs, err := dnsAnswers.lookupIPAddr(ctx, host)
	if err != nil {
		return "", fmt.Errorf("dns: %w", err)
	}
	var v6 netip.Addr
	var first netip.Addr
	for _, ia := range ipaddrs {
		a, ok := netip.AddrFromSlice(ia.IP)
		if !ok {
			continue
		}
		a = a.Unmap()
		if !first.IsValid() {
			first = a
		}
		if !netguard.Public(a) {
			continue
		}
		if a.Is4() {
			return a.String(), nil
		}
		if !v6.IsValid() {
			v6 = a
		}
	}
	if v6.IsValid() {
		return v6.String(), nil
	}
	if first.IsValid() {
		return first.String(), nil
	}
	return "", fmt.Errorf("dns: no address for %s", host)
}

// sameSite reports whether two host names belong to the same registrable domain (eTLD+1).
func sameSite(a, b string) bool { return hostKey(strings.ToLower(a)) == hostKey(strings.ToLower(b)) }

// pageRedirectPolicy is the redirect policy for homepage fetches. A hop is followed only if it stays on the starting
// site, is within maxRedirects, and its path is allowed by the robots.txt of the host it lands on (fetched first when
// that is another host of the same site, e.g. example.com → www.example.com). Every hop is paced like any other
// request to that site. An off-site redirect is not followed: its host is reported through offsite so it can be
// queued and crawled on its own terms. Stopping returns the 3xx response itself (http.ErrUseLastResponse).
func pageRedirectPolicy(startRules *robotstxt.Rules, offsite *string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		from, to := via[0].URL.Hostname(), req.URL.Hostname()
		if !sameSite(from, to) {
			*offsite = strings.ToLower(to)
			return http.ErrUseLastResponse
		}
		if len(via) >= maxRedirects {
			return http.ErrUseLastResponse
		}
		rules := startRules
		if !strings.EqualFold(from, to) {
			sitePace(to, startRules.Delay()) // the robots.txt GET for the new host is a request to the same site
			ip, err := lookupIP(to)          // …and to that host's server, so pace it on the IP gate too
			if err != nil || !netguard.PublicIP(ip) {
				ip = "" // unresolvable or non-public: the dial guard refuses it anyway
			}
			ri := robotsFor(strings.ToLower(to), ip)
			if ri.blockErr != nil {
				return http.ErrUseLastResponse
			}
			rules = ri.rules
		}
		if !rules.Allows(req.URL) {
			return http.ErrUseLastResponse
		}
		sitePace(to, max(startRules.Delay(), rules.Delay())) // the hop is another request to the same site
		return nil
	}
}

// newClient is the robots.txt client. Its redirects follow the same rules as page redirects — same site only, at
// most maxRedirects hops, each paced — without a robots.txt check of their own.
func newClient() *http.Client {
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects || !sameSite(via[0].URL.Hostname(), req.URL.Hostname()) {
				return http.ErrUseLastResponse
			}
			hostThrottle(req.URL.Hostname())
			return nil
		},
	}
}

// setBrowserHeaders sends only the honest, minimal request headers a well-behaved crawler
// needs: our identifying UA plus Accept/Accept-Language so servers negotiate content. We
// deliberately DO NOT send Sec-Fetch-*/Upgrade-Insecure-Requests — those are browser
// client hints, and emitting them under a crawler UA is precisely the header/UA mismatch
// that WAFs (mod_security 920xxx/933xxx anomaly rules) score as a deceptive fake browser
// and then report. A clearly-identified bot with plain headers gets blocked far less.
func setBrowserHeaders(req *http.Request) {
	h := req.Header
	h.Set("User-Agent", userAgent)
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	h.Set("Accept-Language", "en-US,en;q=0.9")
	// Accept-Encoding is intentionally NOT set: Go's transport adds gzip and
	// transparently decompresses it. Setting it ourselves would disable that.
}

// Anti-bot interstitial fingerprints. challengeTitleMarkers are generic phrases only
// trustworthy as a *title* — matching them in body would risk flagging a real page that
// merely mentions the phrase — and only at the START of the title; "exact" ones may be followed by nothing but
// punctuation ("Just a moment..."), so "Just a moment of calm" is a real page. challengeAnyMarkers are vendor
// script/markup fingerprints that never occur in legitimate prose, so we match them in the title OR the body head.
var challengeTitleMarkers = []struct {
	marker, vendor string
	exact          bool
}{
	{"just a moment", "cloudflare", true},
	{"checking your browser", "cloudflare", false},
	{"one moment, please", "challenge", true},
	{"verifying you are human", "challenge", false},
	{"please verify you are a human", "challenge", false},
}

var challengeAnyMarkers = []struct{ marker, vendor string }{
	{"attention required! | cloudflare", "cloudflare"},
	{"enable javascript and cookies to continue", "cloudflare"},
	{"cf-browser-verification", "cloudflare"},
	{"/cdn-cgi/challenge-platform", "cloudflare"},
	{"__cf_chl", "cloudflare"},
	{"pardon our interruption", "imperva"},
	{"_incapsula_resource", "incapsula"},
	{"incapsula incident", "incapsula"},
	{"px-captcha", "perimeterx"},
	{"datadome", "datadome"},
	{"sucuri website firewall", "sucuri"},
}

// detectChallenge returns the anti-bot vendor if the response looks like a JS/CAPTCHA
// interstitial instead of the real page, else "". We don't SOLVE these (that needs a
// real browser) — we just refuse to index the wrong page and record why. Generic phrases
// are matched on the title only; vendor fingerprints anywhere in the title/body head.
func detectChallenge(title string, body []byte) string {
	lt := strings.ToLower(strings.TrimSpace(title))
	for _, s := range challengeTitleMarkers {
		rest, ok := strings.CutPrefix(lt, s.marker)
		if ok && (!s.exact || strings.Trim(rest, " .…!") == "") {
			return s.vendor
		}
	}
	hay := lt
	if len(body) > 0 {
		n := len(body)
		if n > 16384 {
			n = 16384
		}
		hay += "\n" + strings.ToLower(string(body[:n]))
	}
	for _, s := range challengeAnyMarkers {
		if strings.Contains(hay, s.marker) {
			return s.vendor
		}
	}
	return ""
}

// parseRetryAfter reads a 429/503 Retry-After header (delta-seconds or HTTP-date) and returns the wait the server
// asked for (capped at a day so the value stays sane), or 0 if it is absent or unusable. crawlOne only retries when
// the wait is at most throttleWaitMax; a longer one leaves the site alone until a later cycle.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	sane := func(d time.Duration) time.Duration { return min(max(d, 0), 24*time.Hour) }
	if secs, err := strconv.Atoi(v); err == nil {
		return sane(time.Duration(min(secs, 86400)) * time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		return sane(time.Until(t))
	}
	return 0
}

// fetchWebsiteInfo fetches one homepage (https, or http only when https isn't offered) and parses it. rules are the
// robots.txt rules of domain, used to check redirect hops (see pageRedirectPolicy).
func fetchWebsiteInfo(ctx context.Context, domain, ip, etag, lastModified string, rules *robotstxt.Rules) (info *WebsiteInfo, err error) {
	var redirects int
	var offsite string // host of an off-site redirect we stopped at
	defer func() {
		// The off-site redirect target is queued for discovery (same folding as outbound links) so it gets crawled
		// separately, with its own robots.txt and pacing.
		if info == nil || offsite == "" || info.Challenge != "" {
			return
		}
		h := strings.TrimPrefix(offsite, "www.")
		if !crawlSubdomains.Load() {
			h = crawlutil.Registrable(h)
		}
		if crawlutil.Registrable(h) != "" && len(h) <= 253 { // a site, not an IP or a bare public suffix
			if info.outDomains == nil {
				info.outDomains = map[string]bool{}
			}
			info.outDomains[h] = true
		}
	}()
	jar, _ := cookiejar.New(nil) // fresh per-domain jar: lets "set-cookie then redirect" flows reach the real page
	follow := pageRedirectPolicy(rules, &offsite)
	client := &http.Client{
		Transport: transport,
		Jar:       jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			redirects = len(via)
			return follow(req, via)
		},
	}
	var resp *http.Response
	start := time.Now()
	// Both scheme attempts share the SAME ctx deadline (total bounded, not 2x).
	for _, scheme := range []string{"https://", "http://"} {
		if scheme == "http://" {
			// Only fall back when https simply isn't offered (refused / not TLS / bad cert). After a dropped
			// connection, reset or timeout the server is refusing or overloaded — a second request to it is
			// exactly the hammering abuse reports describe. The fallback is a request too: pace it.
			if err != nil && !crawlutil.HTTPSUnavailable(err) {
				break
			}
			ipThrottle(ip)
		}
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, scheme+domain, nil)
		if rerr != nil {
			err = rerr
			continue
		}
		setBrowserHeaders(req)
		// Conditional GET: if we stored validators from the last crawl, ask the server whether
		// the page changed. A 304 lets us skip the download + parse entirely.
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		if lastModified != "" {
			req.Header.Set("If-Modified-Since", lastModified)
		}
		resp, err = client.Do(req)
		if err == nil {
			break
		}
		if ctx.Err() != nil { // deadline blown — don't waste it on the http:// attempt
			break
		}
	}
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return nil, fmt.Errorf("GET failed: %w", err)
	}
	defer resp.Body.Close()

	info = &WebsiteInfo{
		StatusCode:    resp.StatusCode,
		ContentType:   clip(resp.Header.Get("Content-Type"), maxShortLen),
		ContentLength: resp.ContentLength,
		LatencyMs:     latency,
		RedirectCount: redirects,
		ServerHeader:  clip(resp.Header.Get("Server"), maxShortLen),
	}
	// 304 Not Modified: our stored copy is still current. No body to read; flag it and return —
	// the flush path only bumps crawled_at, keeping the row intact.
	if resp.StatusCode == http.StatusNotModified {
		info.NotModified = true
		return info, nil
	}
	// Persist conditional-GET validators only for a genuine 200 — never for error or challenge
	// pages, so a stable error page can't "stick" indefinitely via a future 304 short-circuit.
	// They must be sent back byte-for-byte, so an oversized one is dropped rather than cut.
	if resp.StatusCode == http.StatusOK {
		info.ETag = dropIfLong(resp.Header.Get("ETag"), 256)
		info.LastModified = dropIfLong(resp.Header.Get("Last-Modified"), 64)
	}
	if throttled(info) {
		info.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	}

	// Only parse HTML — store metadata for everything else (no binary-as-HTML).
	if !strings.Contains(strings.ToLower(info.ContentType), "html") {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return info, nil
	}

	// Body-read cap 5MiB→2MiB (2026-08-25): with 24 workers each concurrent html.Parse builds a
	// DOM many× the HTML size, so a 5MiB cap let peak RSS grow past the 256MiB GOMEMLIMIT and swap
	// the 2GB box. Only the title, metadata, links and a word count are kept (never the page text),
	// so 2MiB is ample (>99% of real HTML pages are <2MiB); the tail just gets truncated, not dropped.
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	// An empty body with a text/html Content-Type (common for autodiscover/ftp/email
	// infra hosts, 204s, redirect-to-blank) has nothing to parse. Bail here: on empty
	// input charset.NewReader returns (nil, io.EOF), and html.Parse(nil) then panics
	// with a nil-pointer deref. Keep the metadata we already have.
	if len(bodyBytes) == 0 {
		return info, nil
	}
	// Transcode to UTF-8 from the page's real charset (Content-Type / meta / BOM)
	// so non-UTF-8 pages (Shift-JIS, GBK, EUC-KR, ISO-8859, ...) parse correctly
	// instead of becoming mojibake or being rejected by Postgres on insert.
	utf8Reader, cerr := charset.NewReader(bytes.NewReader(bodyBytes), info.ContentType)
	if cerr != nil || utf8Reader == nil {
		return info, nil // couldn't build a decoder — keep metadata, don't parse a nil reader
	}
	doc, perr := html.Parse(utf8Reader)
	if perr != nil {
		return info, nil // keep metadata; don't fail the whole crawl on a parse hiccup
	}
	parseHTMLInfo(doc, info, domain)
	if v := detectChallenge(info.Title, bodyBytes); v != "" {
		// Hit a bot-challenge/interstitial, not the real page. Record the vendor but
		// DON'T keep the interstitial's title/text/links as if they were content.
		info.Challenge = v
		info.Title, info.H1, info.Description, info.Keywords = "", "", "", ""
		info.OGTitle, info.OGImage, info.Canonical = "", "", ""
		info.ETag, info.LastModified = "", "" // don't let a challenge page's validators trigger a future 304 that keeps it out of a real re-crawl
		info.outDomains = nil
	}
	return info, nil
}

func parseHTMLInfo(root *html.Node, info *WebsiteInfo, domain string) {
	base := strings.ToLower(strings.TrimPrefix(domain, "www."))
	info.outDomains = make(map[string]bool, 16)
	words := 0
	// IG2: collect text for SimHash (capped to bound memory).
	var textBuf strings.Builder
	textBuf.Grow(8192)
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "template", "svg":
				return // skip these subtrees entirely
			case "title":
				if info.Title == "" {
					info.Title = cleanTitle(textOf(n))
				}
			case "h1":
				if info.H1 == "" {
					info.H1 = cleanTitle(textOf(n))
				}
			case "meta":
				readMeta(n, info)
			case "link":
				var rel, href string
				for _, a := range n.Attr {
					switch {
					case strings.EqualFold(a.Key, "rel"):
						rel = a.Val
					case strings.EqualFold(a.Key, "href"):
						href = a.Val
					}
					if rel != "" && href != "" {
						break // a valid link tag carries one rel and one href
					}
				}
				if strings.EqualFold(rel, "canonical") && info.Canonical == "" {
					info.Canonical = dropIfLong(href, maxURLLen)
				}
			case "a":
				for _, a := range n.Attr {
					if !strings.EqualFold(a.Key, "href") {
						continue
					}
					// Only the first href attribute is used (duplicates are invalid HTML).
					href := strings.TrimSpace(a.Val)
					// Skip the expensive url.Parse for relative links ("/path", "#frag")
					// and non-hierarchical schemes ("mailto:", "tel:"): hasHost gates
					// exactly the hrefs that would parse to an empty Host below.
					if hasHost(href) {
						if u, e := url.Parse(href); e == nil && u.Host != "" {
							// Same-site check before allocating the lowercased host: it
							// is only needed on the outbound path (map key + checks).
							w := strings.TrimPrefix(u.Hostname(), "www.")
							if !strings.EqualFold(w, base) {
								host := strings.ToLower(w)
								info.OutboundLinkCount++
								// Collect valid registrable domains for the discovery emit (skip IPs, bare public suffixes,
								// schemeless junk like mailto:/tel:, and over-long hosts).
								// Registrable is computed once: it walks the public-suffix trie, so calling it
								// twice per link (check + fold) doubles that cost.
								if (u.Scheme == "" || u.Scheme == "http" || u.Scheme == "https") &&
									len(host) <= 253 {
									if reg := crawlutil.Registrable(host); reg != "" {
										// Apex-only: FOLD a subdomain to its registrable site (blog.acme.com → acme.com,
										// news.nation.co.ke → nation.co.ke) instead of dropping it — captures the parent apex
										// (breadth) without the fan-out: one apex per family, deduped by the outDomains set.
										emit := host
										if !crawlSubdomains.Load() {
											emit = reg
										}
										info.outDomains[emit] = true
									}
								}
							}
						}
					}
					break
				}
			case "img":
				info.ImageCount++
			case "html":
				for _, a := range n.Attr {
					if strings.EqualFold(a.Key, "lang") && info.PageLanguage == "" {
						info.PageLanguage = clip(a.Val, maxTagLen)
						break
					}
				}
			}
		} else if n.Type == html.TextNode {
			words += countWords(n.Data)
			// IG2: accumulate text for SimHash, capped at 32KB.
			if textBuf.Len() < 32768 {
				textBuf.WriteString(n.Data)
				textBuf.WriteByte(' ')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	info.WordCount = words // body text itself is never stored — only this count
	// IG2: SimHash fingerprint for near-duplicate detection.
	if textBuf.Len() > 0 {
		info.SimHash = simhash.Fingerprint(textBuf.String())
	}
}

// hasHost reports whether href can carry a URL host: it is hierarchical ("://")
// or protocol-relative ("//"). Relative links ("/path", "#frag") and
// non-hierarchical schemes ("mailto:", "tel:", "javascript:", "data:") never do —
// url.Parse returns them with an empty Host — so callers can skip the parse.
func hasHost(href string) bool {
	return strings.Contains(href, "://") || strings.HasPrefix(href, "//")
}

// countWords counts whitespace-separated words like len(strings.Fields(s)), without allocating the slice. Text nodes
// are counted separately (a word never spans two nodes, as before).
func countWords(s string) int {
	n, in := 0, false
	for _, r := range s {
		if unicode.IsSpace(r) {
			in = false
		} else if !in {
			in, n = true, n+1
		}
	}
	return n
}

func readMeta(n *html.Node, info *WebsiteInfo) {
	var name, prop, content, charset string
	for _, a := range n.Attr {
		switch {
		case strings.EqualFold(a.Key, "name"):
			name = strings.ToLower(a.Val)
		case strings.EqualFold(a.Key, "property"):
			prop = strings.ToLower(a.Val)
		case strings.EqualFold(a.Key, "content"):
			content = a.Val
		case strings.EqualFold(a.Key, "charset"):
			charset = a.Val
		}
	}
	set := func(dst *string, v string) {
		if *dst == "" && v != "" {
			*dst = v
		}
	}
	if charset != "" {
		set(&info.Charset, clip(charset, maxTagLen))
	}
	switch name {
	case "description":
		set(&info.Description, clip(content, maxDescLen))
	case "keywords":
		set(&info.Keywords, clip(content, maxKeywordsLen))
	case "robots":
		set(&info.Robots, clip(content, maxShortLen))
	case "twitter:card":
		set(&info.TwitterCard, clip(content, maxTagLen))
	}
	switch prop {
	case "og:description":
		set(&info.Description, clip(content, maxDescLen))
	case "og:title":
		set(&info.OGTitle, cleanTitle(content))
	case "og:image":
		set(&info.OGImage, dropIfLong(content, maxURLLen))
	}
}

// textOf concatenates descendant text (skipping script/style), collapsing
// whitespace in a single pass. The old version built the full string, then
// Fields+Join copied it twice more — 5x the title size on pathological
// inputs (unclosed </title> absorbing the page body).
func textOf(n *html.Node) string {
	var b strings.Builder
	pendingSpace := false
	flushSpace := func() {
		if pendingSpace && b.Len() > 0 {
			b.WriteByte(' ')
		}
		pendingSpace = false
	}
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style") {
			return
		}
		if n.Type == html.TextNode {
			for _, r := range n.Data {
				if unicode.IsSpace(r) {
					pendingSpace = true
				} else {
					flushSpace()
					b.WriteRune(r)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(n)
	return b.String()
}

// robotsInfo is the cached-per-host robots.txt verdict (package robotstxt decides which group applies).
type robotsInfo struct {
	allowed  bool             // the homepage "/" may be fetched
	delay    time.Duration    // Crawl-delay, capped at robotstxt.MaxCrawlDelay
	rules    *robotstxt.Rules // path rules, checked again for every redirect hop
	sitemaps []string         // T1: Sitemap: directive URLs from robots.txt (may be empty)
	blockErr error            // the server dropped/reset/timed out the robots request → it's refusing us (not cached)
}

// robotsCall is one robots.txt fetch in progress; callers asking for the same host meanwhile wait for its result.
type robotsCall struct {
	done chan struct{}
	ri   robotsInfo
}

var robotsInflight sync.Map // host -> *robotsCall

// robotsFor returns the robots.txt verdict for host domain, caching it per host. Concurrent callers for the same
// host share one fetch, so robots.txt is never requested twice at once.
func robotsFor(domain, ip string) robotsInfo {
	if v, ok := robotsCache.Load(domain); ok {
		return v.(robotsInfo)
	}
	c := &robotsCall{done: make(chan struct{})}
	if v, loaded := robotsInflight.LoadOrStore(domain, c); loaded {
		w := v.(*robotsCall)
		<-w.done
		return w.ri
	}
	defer func() { robotsInflight.Delete(domain); close(c.done) }()
	ipThrottle(ip) // the robots.txt GET is a request to that server like any other
	c.ri = fetchRobots(domain, ip)
	if c.ri.blockErr == nil {
		robotsCache.Store(domain, c.ri)
	}
	return c.ri
}

func fetchRobots(domain, ip string) robotsInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	client := newClient()
	for _, scheme := range []string{"https://", "http://"} {
		if scheme == "http://" {
			ipThrottle(ip) // the fallback is a second request to the same server
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+domain+"/robots.txt", nil)
		if err != nil {
			continue
		}
		setBrowserHeaders(req)
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil || !crawlutil.HTTPSUnavailable(err) {
				// timed out, or the server dropped/reset us → it's refusing; stop here (no http:// retry)
				return robotsInfo{allowed: true, blockErr: err}
			}
			continue // https not offered (refused / not TLS / bad cert) → try plain http once
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, robotstxt.MaxBytes))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return robotsInfo{allowed: true} // no robots / not found -> allowed
		}
		ri := parseRobots(string(body))
		ri.sitemaps = sitemapURLsFromRobots(string(body)) // T1: Sitemap: directives for discovery
		return ri
	}
	return robotsInfo{allowed: true} // robots unreachable -> be permissive
}

// parseRobots turns a robots.txt body into the verdict crawlOne uses.
func parseRobots(body string) robotsInfo {
	r := robotstxt.Parse(body)
	return robotsInfo{allowed: r.AllowsPath("/"), delay: r.Delay(), rules: r}
}

// parseTarget turns TARGET into the bare host name to crawl: "example.com", "https://www.example.com/" and
// "EXAMPLE.com" all become "example.com". A path, query, port or user info is rejected: this crawler only fetches
// homepages.
func parseTarget(s string) (string, error) {
	raw := strings.TrimSpace(s)
	s = raw
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("%q is not a domain", raw)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Port() != "" || u.User != nil {
		return "", fmt.Errorf("%q: give a domain only (no path, query or port) — this crawler fetches homepages", raw)
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www."), nil
}
