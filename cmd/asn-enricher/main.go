package main

// asn-enricher — looks up the network (ASN / organisation) and country of each crawled site's primary IPv4 via the
// ipinfo Lite API and stores it in asninfo. website-crawler groups its batches by ASN with it (ASN_MAX_PER_BATCH),
// so one hosting provider never gets a burst of requests.
//
// Needs a free ipinfo Lite token in IPINFO_TOKEN (https://ipinfo.io/signup). Without one it exits and the
// crawler falls back to grouping by IP address, which is still polite but spreads load less evenly.
//
// Domains without an IP yet are skipped until dns-enricher resolves them. Only domains whose IP was actually looked
// up are marked checked: an IP whose lookup failed (rate limit, network) stays in the frontier and is retried, and a
// batch where every lookup failed backs off for ASN_RATELIMIT_BACKOFF_SEC first. Lookups are paced at ASN_RPS
// (1–50 per second). A lookup whose result can't be stored is retried too. SIGTERM / SIGINT stop after the batch in
// flight.
//   IPINFO_TOKEN=xxxx LOOP=1 go run ./cmd/asn-enricher

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lib/pq"

	"cl0q/dbresil"
	"cl0q/internal/config"
	"cl0q/internal/version"
	"cl0q/netguard"
)

// validateConfig checks every knob at startup (NR2). A bad value is a FATAL
// error with a clear message — never a silent clamp or silent default.
// Call this FIRST in main.
func validateConfig() {
	// Polite default: cap the heap at 2GiB. An explicit GOMEMLIMIT env wins.
	config.SetMemoryLimitDefault(2 << 30)

	config.CheckIntRange("ASN_MAXCONNS", config.GetInt("ASN_MAXCONNS", 5), 1, 50)
	config.CheckIntRange("ASN_BATCH", config.GetInt("ASN_BATCH", 1000), 1, 10000)
	config.CheckDurationRange("ASN_IDLE_SEC", time.Duration(config.GetInt("ASN_IDLE_SEC", 60))*time.Second, 0, time.Hour)
	config.CheckDurationRange("ASN_IDLE_MAX_SEC", time.Duration(config.GetInt("ASN_IDLE_MAX_SEC", 1800))*time.Second, time.Second, 24*time.Hour)
	config.CheckDurationRange("ASN_RATELIMIT_BACKOFF_SEC", time.Duration(config.GetInt("ASN_RATELIMIT_BACKOFF_SEC", 300))*time.Second, time.Second, time.Hour)
	config.CheckIntRange("ASN_LIMIT", config.GetInt("ASN_LIMIT", 0), 0, 1000000)
	config.CheckIntRange("ASN_CONCURRENCY", config.GetInt("ASN_CONCURRENCY", 16), 1, 16)
	config.CheckIntRange("ASN_RPS", config.GetInt("ASN_RPS", 15), 1, 50)
	// LOOP is strict-parsed so a typo can't silently change run mode.
	if raw := os.Getenv("LOOP"); raw != "" && raw != "0" && raw != "1" {
		log.Fatalf("invalid LOOP=%q: must be 0 or 1", raw)
	}
}

// LiteResponse matches the ipinfo LITE API (https://api.ipinfo.io/lite/<ip>):
// ASN + country + continent. (The free Lite tier has no city/region/org precision —
// that's the paid Core/Standard API.)
type LiteResponse struct {
	IP            string `json:"ip"`
	ASN           string `json:"asn"`
	AsName        string `json:"as_name"`
	AsDomain      string `json:"as_domain"`
	CountryCode   string `json:"country_code"`
	Country       string `json:"country"`
	ContinentCode string `json:"continent_code"`
	Continent     string `json:"continent"`
}

func main() {
	// -version before anything else (full provenance report).
	if version.HandleVersionFlag("asn-enricher") {
		return
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	validateConfig() // NR2: refuse to start on any invalid knob, before anything else runs
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	log.Println("Starting asn-enricher (domain_info primary-IP enrichment)")

	// ---------------- db ----------------
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = "postgres://cl0q:cl0q@localhost:5432/cl0q?sslmode=disable"
	}
	db, err := sql.Open("postgres", dbresil.WithConnectTimeout(connStr, 10))
	if err != nil {
		log.Fatalf("Error opening database: %v", err)
	}
	defer db.Close()
	// Shared server has max_connections=100 — cap our pool so we never contribute
	// to "too many clients". Failed inserts just retry next loop (IP stays un-enriched).
	db.SetMaxOpenConns(envInt("ASN_MAXCONNS", 5))
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	if dbresil.WaitForRemoteCtx(ctx, db) != nil { // ride out a database outage at startup instead of crash-looping
		return
	}
	log.Println("Database connection established")

	// asninfo is also created by initdb/01-schema.sql; ensure it exists for bare `go run` too.
	if _, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS asninfo (
			ip TEXT PRIMARY KEY, loc TEXT, org TEXT, city TEXT, postal TEXT, region TEXT,
			country TEXT, timezone TEXT, continent_code TEXT, continent_name TEXT,
			country_flag_emoji TEXT, country_flag_unicode TEXT, country_name TEXT,
			country_currency_code TEXT, country_currency_symbol TEXT, country_flag_url TEXT
		)`); err != nil {
		log.Fatalf("Failed to ensure 'asninfo' table: %v", err)
	}

	// ---------------- ipinfo LITE client ----------------
	token := os.Getenv("IPINFO_TOKEN")
	if token == "" {
		// Exit 0 (docker-compose.yml restarts this service only on failure): without ASN data website-crawler
		// groups its batches by IP address instead.
		log.Println("IPINFO_TOKEN is not set — ASN enrichment disabled. Get a free ipinfo Lite token at https://ipinfo.io/signup")
		return
	}
	// The token goes to api.ipinfo.io only: redirects are never followed (a 3xx is a failed lookup, retried later).
	// The ipinfo API is public; the netguard SSRF guard on the transport
	// refuses any dial to a non-public address. Timeouts and redirect
	// policy are unchanged — only the dial Control hook is added.
	httpClient := &http.Client{Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Control: netguard.Control}).DialContext,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// ---------------- continuous loop (service) or one-shot ----------------
	// LOOP=0 must mean one-shot (an earlier `!= ""` check treated "0" as truthy and looped forever).
	if v := os.Getenv("LOOP"); v != "" && v != "0" {
		batchN := envInt("ASN_BATCH", 1000)
		idle := time.Duration(envInt("ASN_IDLE_SEC", 60)) * time.Second
		idleMax := time.Duration(envInt("ASN_IDLE_MAX_SEC", 1800)) * time.Second // cap when caught up
		backoff := time.Duration(envInt("ASN_RATELIMIT_BACKOFF_SEC", 300)) * time.Second
		log.Printf("LOOP mode: batch=%d idle=%s..%s rate-limit-backoff=%s", batchN, idle, idleMax, backoff)
		// fetchIPs is a full-table merge scan of domain_info. Re-running it every idle is only
		// worth it while DRAINING a backlog. A small batch (< batchN/2) means we're caught up to
		// the steady trickle of newly-resolved IPs — process those, then back off exponentially
		// toward idleMax so the heavy scan runs occasionally, not every minute. A full-ish batch
		// means a real backlog, so keep polling fast until it's drained.
		curIdle := idle
		grow := func() {
			if curIdle *= 2; curIdle > idleMax {
				curIdle = idleMax
			}
		}
		for ctx.Err() == nil {
			ips, ids, byIP := fetchIPs(db, batchN)
			if len(ids) == 0 {
				log.Printf("frontier empty; sleeping %s", curIdle)
				dbresil.SleepCtx(ctx, curIdle)
				grow()
				continue
			}
			var ins, reg int
			var failedIPs map[string]bool
			if len(ips) > 0 {
				ins, reg, failedIPs = enrichBatch(db, httpClient, token, ips)
				if ins == 0 && len(failedIPs) > 0 {
					// Nothing written AND at least one ipinfo lookup errored → likely rate limit/outage.
					// Back off WITHOUT stamping so these ids retry (health check flags it too).
					// NOTE: ins==0 with no failures means every IP was ALREADY in asninfo (ON CONFLICT
					// DO NOTHING) — NOT a rate limit. Fall through and stamp so the batch leaves the
					// frontier; otherwise those ids recycle forever and stall the enricher for 5m each.
					log.Printf("0 enriched, %d/%d ipinfo lookups failed — likely rate limit; backing off %s", len(failedIPs), len(ips), backoff)
					dbresil.SleepCtx(ctx, backoff)
					continue
				}
			}
			// Clear the batch from the frontier, except domains whose IP lookup failed: those are retried.
			stamped := markChecked(db, withoutFailed(ids, byIP, failedIPs))
			log.Printf("batch done: enriched=%d (withGeo=%d) of %d ips; %d domains stamped", ins, reg, len(ips), stamped)
			if len(ids) >= batchN/2 {
				curIdle = idle // real backlog → keep draining fast
			} else {
				// caught up to a trickle → don't re-poll immediately
				dbresil.SleepCtx(ctx, curIdle)
				grow()
			}
		}
		log.Println("stopped: shutdown signal received")
		return
	}

	// one-shot (optional ASN_LIMIT)
	ips, ids, byIP := fetchIPs(db, envInt("ASN_LIMIT", 0))
	if len(ids) == 0 {
		log.Println("Nothing to do. Exiting.")
		return
	}
	ins, reg, failedIPs := enrichBatch(db, httpClient, token, ips)
	stamped := markChecked(db, withoutFailed(ids, byIP, failedIPs))
	log.Printf("Done. inserted=%d (withGeo=%d) of %d fetched; %d stamped", ins, reg, len(ips), stamped)
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			return n
		}
	}
	return def
}

// fetchIPs returns the un-enriched frontier: domains flagged asn_checked_at IS NULL that have a
// primary IPv4. It returns the DISTINCT IPs to look up, every domain id in the batch (so the
// caller can stamp them checked afterwards) and which ids share each IP. Reads via idx_domain_info_asn_todo — a tiny partial
// index over just the frontier — so this is O(batch), never a full-table scan of domain_info.
func fetchIPs(db *sql.DB, limit int) (ips []string, ids []int64, byIP map[string][]int64) {
	query := `
		SELECT di.id, btrim(split_part(di.ip_addresses_ipv4, ',', 1)) AS ip
		FROM domain_info di
		WHERE di.asn_checked_at IS NULL
		  AND di.ip_addresses_ipv4 IS NOT NULL
		ORDER BY di.id`
	if limit > 0 {
		query += " LIMIT " + strconv.Itoa(limit)
	}
	rows, err := db.Query(query)
	if err != nil {
		log.Printf("query IPs: %v", err)
		return nil, nil, nil
	}
	defer rows.Close()
	byIP = make(map[string][]int64)
	for rows.Next() {
		var id int64
		var ip string
		if err := rows.Scan(&id, &ip); err != nil {
			log.Printf("scan IP: %v", err)
			return ips, ids, byIP
		}
		ids = append(ids, id)            // stamp EVERY fetched domain, even ones with no usable IP
		if ip == "" || ip == "_ERROR_" { // "_ERROR_" is a placeholder some writers store for a failed lookup, not an address
			continue // nothing to look up, but the id still gets stamped so it leaves the frontier
		}
		if _, dup := byIP[ip]; !dup {
			ips = append(ips, ip)
		}
		byIP[ip] = append(byIP[ip], id)
	}
	if err := rows.Err(); err != nil {
		// Surface a mid-scan error instead of swallowing it. A short batch is harmless here —
		// the unread ids keep asn_checked_at IS NULL and are picked up on the next scan.
		log.Printf("fetchIPs rows: %v", err)
	}
	return ips, ids, byIP
}

// withoutFailed drops the domains whose IP lookup failed, so they stay in the frontier and are retried.
func withoutFailed(ids []int64, byIP map[string][]int64, failed map[string]bool) []int64 {
	if len(failed) == 0 {
		return ids
	}
	skip := make(map[int64]bool)
	for ip := range failed {
		for _, id := range byIP[ip] {
			skip[id] = true
		}
	}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !skip[id] {
			out = append(out, id)
		}
	}
	return out
}

// markChecked stamps asn_checked_at=now() on the fetched domain ids so they leave the frontier
// (partial index) and are never re-scanned. Targeted by id (≤batch rows) — cheap, no full scan.
func markChecked(db *sql.DB, ids []int64) int64 {
	if len(ids) == 0 {
		return 0
	}
	res, err := db.Exec(`UPDATE domain_info SET asn_checked_at=now() WHERE id = ANY($1) AND asn_checked_at IS NULL`, pq.Array(ids))
	if err != nil {
		log.Printf("markChecked: %v", err)
		return 0
	}
	n, _ := res.RowsAffected()
	return n
}

// enrichBatch looks up each IP via the ipinfo LITE API (concurrently) and upserts
// ASN/country/continent into asninfo. Returns (rows inserted, rows with a country, the IPs whose
// lookup errored: 429/non-200/network). The caller uses the failures to tell a real rate limit
// apart from an all-already-known batch (none failed, inserted==0 via ON CONFLICT DO NOTHING),
// and to keep the failed IPs' domains in the frontier.
// Lite has no city/region, so we store the country as the geo and "ASN as_name" as org.
func enrichBatch(db *sql.DB, hc *http.Client, token string, ips []string) (inserted, withGeo int, failed map[string]bool) {
	const insertQuery = `
		INSERT INTO asninfo (ip, org, region, country, continent_code, continent_name, country_name)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (ip) DO NOTHING`
	var mu sync.Mutex
	var wg sync.WaitGroup
	failed = make(map[string]bool)
	sem := make(chan struct{}, min(max(envInt("ASN_CONCURRENCY", 16), 1), 16)) // cap concurrent Lite lookups (1–16)
	// Pace requests to stay UNDER ipinfo's rate limit — a steady RPS avoids the
	// burst→429→5-min-backoff cycle that was stalling enrichment. ASN_RPS is kept within 1–50.
	tk := time.NewTicker(time.Second / time.Duration(min(max(envInt("ASN_RPS", 15), 1), 50)))
	defer tk.Stop()
	rl := tk.C
	for _, ip := range ips {
		wg.Add(1)
		sem <- struct{}{}
		go func(ip string) {
			defer wg.Done()
			defer func() { <-sem }()
			markFailed := func() { mu.Lock(); failed[ip] = true; mu.Unlock() }
			defer func() { // one bad response must not kill the process; the IP is retried next batch
				if r := recover(); r != nil {
					log.Printf("lite lookup %s: panic: %v", ip, r)
					markFailed()
				}
			}()
			<-rl // throttle: at most ASN_RPS lookups/sec
			lr, err := lookupLite(hc, token, ip)
			if err != nil {
				log.Printf("lite lookup %s: %v", ip, err)
				markFailed()
				return
			}
			org := strings.TrimSpace(lr.ASN + " " + lr.AsName)
			// region <- country name, country <- 2-letter code (matches existing rows)
			res, err := db.Exec(insertQuery, ip, org, lr.Country, lr.CountryCode, lr.ContinentCode, lr.Continent, lr.Country)
			if err != nil {
				log.Printf("insert %s: %v — will retry", ip, err)
				markFailed() // not stored → keep its domains in the frontier
				return
			}
			n, _ := res.RowsAffected()
			mu.Lock()
			if n > 0 {
				inserted++
			}
			if lr.Country != "" {
				withGeo++
			}
			mu.Unlock()
		}(ip)
	}
	wg.Wait()
	return inserted, withGeo, failed
}

// lookupLite queries the ipinfo Lite API for a single IP.
func lookupLite(hc *http.Client, token, ip string) (*LiteResponse, error) {
	req, err := http.NewRequest(http.MethodGet, "https://api.ipinfo.io/lite/"+ip, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// 429 / "rate limit" here is what the health check greps for.
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var r LiteResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
