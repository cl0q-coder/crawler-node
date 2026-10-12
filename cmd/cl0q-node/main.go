package main

// cl0q-node — sends what this node crawled to cl0q.
//
// The node's crawlers write to its OWN local Postgres. This forwarder tails NEW rows of the local website_info /
// domain_info and POSTs them to cl0q's ingest API (https://cl0q.com/api/ingest/*) with the node's bearer token.
// cl0q checks the token AND the source IP it was enrolled with, so a stolen token is useless elsewhere.
//
// Idempotent + resumable: the ingest deduplicates (upsert / NOT EXISTS), and the local cl0q_node_state table holds
// a per-table id watermark, so a restart resumes where it left off and a failed POST is retried next cycle.
// A batch the ingest rejects for good (a 4xx other than 401/403/408/429) is moved to the local
// cl0q_node_deadletter table and skipped, so one bad batch can't hold back everything behind it; auth errors,
// rate limits, 5xx and network errors are retried.
// Known limit: the watermark is the row id, so a page re-crawled in place (same id) is not re-sent.
// Not sent at all: bot-challenge interstitials and names that resolved only to non-public addresses.
//
// INGEST_URL must be https (the token travels in every request); CL0Q_NODE_ALLOW_HTTP=1 allows http for a local
// test setup. Redirects are never followed. SIGTERM / SIGINT stop after the batch in flight.
//
//   LOCAL_DATABASE_URL=postgres://cl0q:cl0q@localhost:5432/cl0q?sslmode=disable \
//   INGEST_URL=https://cl0q.com NODE_TOKEN=... CL0Q_NODE_ID=my-node \
//   cl0q-node [-batch 1000] [-interval 60s] [-once] [-dry-run]

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"cl0q/botid"
	"cl0q/internal/config"
	"cl0q/internal/metrics"
	"cl0q/internal/version"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// maxBatch is the most rows the ingest accepts per request.
const maxBatch = 2000

var (
	batch    = flag.Int("batch", 1000, "rows per POST (1–2000; the ingest accepts at most 2000)")
	interval = flag.Duration("interval", 60*time.Second, "pause between forward cycles")
	once     = flag.Bool("once", false, "run a single cycle and exit")
	dryRun   = flag.Bool("dry-run", false, "read + build batches but do not POST and do not move the watermark (for testing)")
)

type wiRow struct {
	Domain        string `json:"domain"`
	Title         string `json:"title,omitempty"`
	Description   string `json:"description,omitempty"`
	Keywords      string `json:"keywords,omitempty"`
	Canonical     string `json:"canonical,omitempty"`
	H1            string `json:"h1,omitempty"`
	StatusCode    int64  `json:"status_code"`
	ContentType   string `json:"content_type,omitempty"`
	ContentLength int64  `json:"content_length"`
	LatencyMs     int64  `json:"latency_ms"`
	Redirects     int64  `json:"redirects"`
	OutboundLinks int64  `json:"outbound_links"`
	ImageCount    int64  `json:"image_count"`
	WordCount     int64  `json:"word_count"`
	Charset       string `json:"charset,omitempty"`
	Robots        string `json:"robots,omitempty"`
	OGTitle       string `json:"og_title,omitempty"`
	OGImage       string `json:"og_image,omitempty"`
	TwitterCard   string `json:"twitter_card,omitempty"`
	PageLanguage  string `json:"page_language,omitempty"`
	ServerHeader  string `json:"server_header,omitempty"`
	FetchError    string `json:"fetch_error,omitempty"`
	ETag          string `json:"etag,omitempty"`
	LastModified  string `json:"last_modified,omitempty"`
	Simhash       int64  `json:"simhash,omitempty"`
	DuplicateOf   string `json:"duplicate_of,omitempty"`
}

type diRow struct {
	Domain  string `json:"domain"`
	V4      string `json:"ipv4,omitempty"`
	V6      string `json:"ipv6,omitempty"`
	MX      string `json:"mx,omitempty"`
	CNAME   string `json:"cname,omitempty"`
	FoundOn string `json:"found_on,omitempty"`
}

// checkIngestURL accepts an absolute https URL (http only when allowHTTP), so the bearer token is never sent in clear
// text by mistake.
func checkIngestURL(raw string, allowHTTP bool) (string, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("INGEST_URL %q is not an absolute URL", raw)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && allowHTTP:
		log.Printf("WARNING: INGEST_URL is plain http — the node token is sent unencrypted (CL0Q_NODE_ALLOW_HTTP=1)")
	default:
		return "", fmt.Errorf("INGEST_URL must be https (got %q); set CL0Q_NODE_ALLOW_HTTP=1 only for a local test setup", u.Scheme)
	}
	return u.String(), nil
}

// validateConfig checks every knob at startup (NR2). A bad value is a FATAL
// error with a clear message — never a silent clamp. Call this FIRST in
// main, after flag.Parse.
func validateConfig() {
	// Polite default: cap the heap at 2GiB. An explicit GOMEMLIMIT env wins.
	config.SetMemoryLimitDefault(2 << 30)

	// The ingest accepts at most 2000 rows per request; more is rejected
	// server-side, fewer than 1 is nonsense.
	config.CheckIntRange("batch", *batch, 1, maxBatch)
	config.CheckDurationRange("interval", *interval, time.Second, time.Hour)
	// CL0Q_NODE_ALLOW_HTTP is strict-parsed so a typo can't silently
	// downgrade the token to cleartext http.
	if raw := os.Getenv("CL0Q_NODE_ALLOW_HTTP"); raw != "" && raw != "0" && raw != "1" {
		log.Fatalf("invalid CL0Q_NODE_ALLOW_HTTP=%q: must be 0 or 1", raw)
	}
}

func main() {
	// -version before anything else (full provenance report).
	if version.HandleVersionFlag("cl0q-node") {
		return
	}
	// Subcommand before flag parsing: `cl0q-node enroll` (PROTOCOL.md §1).
	if len(os.Args) > 1 && os.Args[1] == "enroll" {
		runEnrollCommand(os.Args[2:])
		return
	}
	// OP2: `cl0q-node status` — operator snapshot (queries METRICS_ADDR).
	if len(os.Args) > 1 && os.Args[1] == "status" {
		runStatusCommand(os.Args[2:])
		return
	}
	// Track U rung U2: `cl0q-node update` — signed-manifest update check.
	if len(os.Args) > 1 && os.Args[1] == "update" {
		runUpdateCommand(os.Args[2:])
		return
	}
	flag.Parse()
	validateConfig() // NR2: refuse to start on any invalid knob, before anything else runs
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	// Credentials: NODE_TOKEN/CL0Q_NODE_ID env win; otherwise the enrol
	// credential file (written by `cl0q-node enroll`, mode 0600).
	creds, err := loadCredentials()
	if err != nil {
		log.Fatal(err)
	}
	credNodeID, credToken := creds.NodeID, creds.Token
	if os.Getenv(botid.EnvNodeID) == "" && credNodeID != "" {
		os.Setenv(botid.EnvNodeID, credNodeID)
	}
	userAgent = config.PublicUserAgent() // truncated operator prefix, PROTOCOL.md §4
	localDSN := env("LOCAL_DATABASE_URL", env("DATABASE_URL", ""))
	token := env("NODE_TOKEN", "")
	if token == "" {
		token = credToken
	}
	if localDSN == "" || (!*dryRun && (env("INGEST_URL", "") == "" || token == "")) {
		log.Fatal("need LOCAL_DATABASE_URL, and INGEST_URL + NODE_TOKEN (unless -dry-run)")
	}
	ingestURL := ""
	if !*dryRun {
		var err error
		if ingestURL, err = checkIngestURL(env("INGEST_URL", ""), env("CL0Q_NODE_ALLOW_HTTP", "") == "1"); err != nil {
			log.Fatal(err)
		}
	}

	db, err := sql.Open("postgres", localDSN)
	if err != nil {
		log.Fatalf("open local db: %v", err)
	}
	db.SetMaxOpenConns(4)
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS cl0q_node_state (name TEXT PRIMARY KEY, last_id BIGINT NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS cl0q_node_deadletter (id BIGSERIAL PRIMARY KEY, tbl TEXT NOT NULL,
			first_id BIGINT NOT NULL, last_id BIGINT NOT NULL, status INT NOT NULL, error TEXT, rows JSONB,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			log.Fatalf("ensure node tables: %v", err)
		}
	}

	st := pgStore{db}
	// NR5: the token lives in a mutable holder so rotation doesn't need a
	// restart. Auto-rotation is only possible when the token came from the
	// credential file (an env token can't be rotated in place).
	credPath := ""
	if env("NODE_TOKEN", "") == "" && credToken != "" {
		credPath = credentialsPath()
	}
	ts := &tokenSource{token: token, refreshSecret: creds.RefreshSecret, nodeID: os.Getenv(botid.EnvNodeID), credPath: credPath, ingestURL: ingestURL}
	newSender := func(tok string) sender { return httpSender(newIngestClient(), ingestURL, tok) }
	// NR6: observability.
	nmreg := metrics.NewRegistry()
	nmForwarded := nmreg.Counter("node_rows_forwarded_total", "rows successfully sent to the ingest")
	nmFailed := nmreg.Counter("node_ingest_failures_total", "ingest POST failures (retried)")
	nmDeadLettered := nmreg.Counter("node_rows_deadlettered_total", "rows dead-lettered after permanent rejection")
	nmRateLimited := nmreg.Counter("node_ingest_rate_limited_total", "ingest 429/503 responses (Retry-After honored)")
	nmAuthState := nmreg.Gauge("node_auth_state", "0=ok, 1=auth failing (backoff), 2=token revoked")
	nmreg.GaugeFunc("node_watermark_lag_website", "unforwarded rows in website_info", func() int64 {
		return watermarkLag(db, "website_info")
	})
	nmreg.GaugeFunc("node_watermark_lag_domain", "unforwarded rows in domain_info", func() int64 {
		return watermarkLag(db, "domain_info")
	})
	nmreg.GaugeFunc("node_db_bytes", "local database size in bytes", func() int64 {
		var n int64
		_ = db.QueryRow(`SELECT pg_database_size(current_database())`).Scan(&n)
		return n
	})
	if addr := os.Getenv("METRICS_ADDR"); addr != "" {
		nmreg.Serve(addr)
		log.Printf("metrics: serving /metrics and /status on %s", addr)
	}
	send := authSender(ts, newSender)
	// NR6: count forward outcomes.
	// A1: rate-limit gate — honors the ingest's Retry-After so a 429'd node
	// stops hammering instead of retrying on the next tight cycle.
	gate := &rateGate{}
	countedSend := func(path string, rows any) error {
		err := send(path, rows)
		if err == nil {
			switch v := rows.(type) {
			case []wiRow:
				nmForwarded.Add(int64(len(v)))
			case []diRow:
				nmForwarded.Add(int64(len(v)))
			}
		} else {
			nmFailed.Add(1)
			var se *statusError
			if errors.As(err, &se) && (se.status == http.StatusTooManyRequests || se.status == http.StatusServiceUnavailable) {
				nmRateLimited.Add(1)
				d := se.retryAfter
				if d <= 0 {
					// No header: back off one interval rather than spinning.
					d = *interval
				}
				gate.note(d)
				log.Printf("ingest rate-limited (status %d): holding off sends for %v", se.status, d)
			}
			if _, dead := permanent(err); dead {
				// The batch was dead-lettered (forward moves it there on
				// permanent rejection); count the rows.
				switch v := rows.(type) {
				case []wiRow:
					nmDeadLettered.Add(int64(len(v)))
				case []diRow:
					nmDeadLettered.Add(int64(len(v)))
				}
			}
		}
		return err
	}
	// NR6: reflect auth state in the gauge.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				nmAuthState.Store(ts.authState())
			}
		}
	}()
	for {
		wi := forward(ctx, st, countedSend, "website_info", "/api/ingest/website", *batch, *dryRun)
		di := forward(ctx, st, countedSend, "domain_info", "/api/ingest/domains", *batch, *dryRun)
		log.Printf("cycle done: website_info forwarded=%d, domain_info forwarded=%d", wi, di)
		if *once {
			return
		}
		// NR5: after auth failures, back off longer than the normal interval
		// instead of spinning on a dead token. A1: same for ingest rate
		// limits — the gate holds until Retry-After elapses.
		wait := *interval
		if ab := ts.authBackoff(); ab > wait {
			wait = ab
		}
		if rb := gate.remaining(); rb > wait {
			wait = rb
		}
		if !sleepCtx(ctx, wait) {
			if ctx.Err() != nil {
				log.Printf("stopped: shutdown signal received")
			}
			return
		}
	}
}

// watermarkLag returns how many rows are waiting to be forwarded (NR6).
func watermarkLag(db *sql.DB, table string) int64 {
	var mark int64
	_ = db.QueryRow(`SELECT last_id FROM cl0q_node_state WHERE name=$1`, table).Scan(&mark)
	var maxID int64
	q := `SELECT COALESCE(MAX(id), 0) FROM website_info`
	if table == "domain_info" {
		q = `SELECT COALESCE(MAX(id), 0) FROM domain_info`
	}
	_ = db.QueryRow(q).Scan(&maxID)
	if maxID < mark {
		return 0
	}
	return maxID - mark
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// store is the local state forward needs; pgStore implements it on the node's Postgres (tests use a fake).
type store interface {
	watermark(table string) (int64, error)
	readBatch(table string, afterID int64, limit int) (rows any, published, scanned int, maxID int64, err error)
	setWatermark(table string, id int64) error
	deadLetter(table string, firstID, lastID int64, status int, msg string, rows any) error
}

// sender POSTs one batch to an ingest path.
type sender func(path string, rows any) error

// statusError is a non-200 answer from the ingest.
type statusError struct {
	status int
	body   string
	// retryAfter is how long the server asked us to wait (Retry-After on
	// 429/503), parsed at response time. Zero when the server sent none.
	retryAfter time.Duration
}

func (e *statusError) Error() string { return fmt.Sprintf("status %d: %s", e.status, e.body) }

// parseRetryAfter interprets a Retry-After header value per RFC 7231 §7.1.3:
// either delay-seconds or an HTTP-date. Out-of-range or unparseable values
// yield 0 (caller falls back to the normal interval).
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}

// rateGate holds the ingest-imposed hold-off: when the server answers 429 or
// 503 with Retry-After, forward loops must not fire again until it elapses.
// A1: without this, a 429'd node retries on the next tight cycle and keeps
// hammering the ingest it was told to leave alone.
type rateGate struct {
	mu    sync.Mutex
	until time.Time
}

// maxRateBackoff caps a single Retry-After note: a quota 429 may say 24h,
// but sleeping the whole forwarder that long hides operator errors. The
// server re-429s on the next cycle if the quota is still spent.
const maxRateBackoff = time.Hour

// note records a hold-off of d (plus jitter so many 429'd nodes don't
// retry in lockstep). Keeps the latest (furthest) deadline.
func (g *rateGate) note(d time.Duration) {
	if d <= 0 {
		return
	}
	if d > maxRateBackoff {
		d = maxRateBackoff
	}
	jitter := time.Duration(rand.Int63n(int64(5 * time.Second)))
	deadline := time.Now().Add(d + jitter)
	g.mu.Lock()
	if deadline.After(g.until) {
		g.until = deadline
	}
	g.mu.Unlock()
}

// remaining is how long the forwarder must still wait. Zero when no
// hold-off is in effect.
func (g *rateGate) remaining() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Until(g.until)
}

// permanent reports whether a POST error means the ingest will never accept this batch as sent: a 4xx other than
// 401/403 (token or source IP — fix the node, then the same batch goes through), 408 or 429 (try later).
func permanent(err error) (status int, ok bool) {
	var se *statusError
	if !errors.As(err, &se) {
		return 0, false
	}
	switch {
	case se.status == 401, se.status == 403, se.status == 408, se.status == 429:
		return se.status, false
	case se.status >= 400 && se.status < 500:
		return se.status, true
	}
	return se.status, false
}

// forward tails new rows of one table past its watermark and POSTs them in batches. It stops when ctx ends (after
// the batch in flight), on a read error, or on a POST error worth retrying.
func forward(ctx context.Context, st store, send sender, table, path string, limit int, dryRun bool) int {
	last, err := st.watermark(table)
	if err != nil {
		// Never fall back to 0 on a read error: that would re-send the whole table, and website rows are upserts,
		// so old content could overwrite newer rows at cl0q.
		log.Printf("%s watermark: %v — skipping this cycle", table, err)
		return 0
	}
	total := 0
	for ctx.Err() == nil {
		rows, published, scanned, maxID, err := st.readBatch(table, last, limit)
		if err != nil {
			log.Printf("%s read: %v", table, err)
			return total
		}
		if scanned == 0 {
			return total
		}
		if published > 0 { // domain_info skips null-domain rows, so a page can scan but publish nothing
			if dryRun {
				log.Printf("[dry-run] %s: would POST %d row(s) up to id %d (e.g. %v)", table, published, maxID, firstDomain(rows))
			} else if err := send(path, rows); err != nil {
				status, dead := permanent(err)
				if !dead {
					log.Printf("%s POST: %v — will retry next cycle", table, err)
					return total
				}
				// The ingest refuses this batch for good: keep it locally for inspection and move on.
				if derr := st.deadLetter(table, last+1, maxID, status, err.Error(), rows); derr != nil {
					log.Printf("%s POST rejected (%v) and dead-letter write failed: %v — will retry next cycle", table, err, derr)
					return total
				}
				log.Printf("%s POST rejected for good (%v): %d row(s), ids %d–%d moved to cl0q_node_deadletter", table, err, published, last+1, maxID)
				published = 0
			}
		}
		// Advance the stored watermark only after a successful POST or a dead-letter. A dry run walks the table in
		// memory but never stores its position, so a test run can't make the next real run skip rows.
		if !dryRun {
			if err := st.setWatermark(table, maxID); err != nil {
				log.Printf("%s watermark: %v", table, err)
				return total
			}
		}
		last = maxID
		total += published
		if scanned < limit {
			return total
		}
	}
	return total
}

type pgStore struct{ db *sql.DB }

func (p pgStore) watermark(table string) (int64, error) {
	var last int64
	err := p.db.QueryRow(`SELECT last_id FROM cl0q_node_state WHERE name=$1`, table).Scan(&last)
	if err == sql.ErrNoRows {
		return 0, nil // first run
	}
	return last, err
}

func (p pgStore) setWatermark(table string, id int64) error {
	_, err := p.db.Exec(`INSERT INTO cl0q_node_state (name,last_id) VALUES ($1,$2)
		ON CONFLICT (name) DO UPDATE SET last_id=EXCLUDED.last_id`, table, id)
	return err
}

func (p pgStore) deadLetter(table string, firstID, lastID int64, status int, msg string, rows any) error {
	b, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	_, err = p.db.Exec(`INSERT INTO cl0q_node_deadletter (tbl, first_id, last_id, status, error, rows) VALUES ($1,$2,$3,$4,$5,$6)`,
		table, firstID, lastID, status, msg, string(b))
	return err
}

func (p pgStore) readBatch(table string, afterID int64, limit int) (rowsOut any, published, scanned int, maxID int64, err error) {
	db := p.db
	switch table {
	case "website_info":
		q := `SELECT id,domain,title,description,keywords,canonical,h1,status_code,content_type,
			content_length,latency_ms,redirects,outbound_links,image_count,word_count,charset,robots,
			og_title,og_image,twitter_card,page_language,server_header,fetch_error,etag,last_modified,
			simhash,duplicate_of
			FROM website_info WHERE id > $1 ORDER BY id LIMIT $2`
		rs, e := db.Query(q, afterID, limit)
		if e != nil {
			return nil, 0, 0, afterID, e
		}
		defer rs.Close()
		out := []wiRow{}
		for rs.Next() {
			var id int64
			var d string
			var t, de, k, ca, h1, ct, ch, ro, ogt, ogi, tw, pl, sh, fe, et, lm, dup sql.NullString
			var sc, cl, lt, rd, ob, ic, wc, sim sql.NullInt64
			if e := rs.Scan(&id, &d, &t, &de, &k, &ca, &h1, &sc, &ct, &cl, &lt, &rd, &ob, &ic, &wc,
				&ch, &ro, &ogt, &ogi, &tw, &pl, &sh, &fe, &et, &lm, &sim, &dup); e != nil {
				return nil, 0, 0, afterID, e
			}
			scanned++
			maxID = id
			if !sendable(fe.String) {
				continue
			}
			out = append(out, wiRow{
				Domain: d, Title: t.String, Description: de.String, Keywords: k.String, Canonical: ca.String,
				H1: h1.String, StatusCode: sc.Int64, ContentType: ct.String, ContentLength: cl.Int64,
				LatencyMs: lt.Int64, Redirects: rd.Int64, OutboundLinks: ob.Int64, ImageCount: ic.Int64,
				WordCount: wc.Int64, Charset: ch.String, Robots: ro.String, OGTitle: ogt.String,
				OGImage: ogi.String, TwitterCard: tw.String, PageLanguage: pl.String, ServerHeader: sh.String,
				FetchError: fe.String, ETag: et.String, LastModified: lm.String,
				Simhash: sim.Int64, DuplicateOf: dup.String,
			})
		}
		return out, len(out), scanned, maxID, rs.Err()
	case "domain_info":
		rs, e := db.Query(`SELECT id,domain,ip_addresses_ipv4,ip_addresses_ipv6,mx_records,cname,found_on
			FROM domain_info WHERE id > $1 ORDER BY id LIMIT $2`, afterID, limit)
		if e != nil {
			return nil, 0, 0, afterID, e
		}
		defer rs.Close()
		out := []diRow{}
		for rs.Next() {
			var id int64
			var d, v4, v6, mx, cn, fo sql.NullString
			if e := rs.Scan(&id, &d, &v4, &v6, &mx, &cn, &fo); e != nil {
				return nil, 0, 0, afterID, e
			}
			scanned++
			maxID = id
			if d.String == "" {
				continue
			}
			out = append(out, diRow{Domain: d.String, V4: v4.String, V6: v6.String, MX: mx.String, CNAME: cn.String, FoundOn: fo.String})
		}
		return out, len(out), scanned, maxID, rs.Err()
	}
	return nil, 0, 0, afterID, fmt.Errorf("unknown table %s", table)
}

// sendable reports whether a website_info row is worth sending. Not sent: bot-challenge interstitials (their content
// was discarded at fetch time, so there is nothing to index) and names that resolved only to non-public addresses
// (never contacted).
func sendable(fetchError string) bool {
	return !strings.HasPrefix(fetchError, "challenge:") && fetchError != "blocked:non-public-address"
}

func firstDomain(rows any) string {
	switch r := rows.(type) {
	case []wiRow:
		if len(r) > 0 {
			return r[0].Domain
		}
	case []diRow:
		if len(r) > 0 {
			return r[0].Domain
		}
	}
	return ""
}

// userAgent identifies the node to the ingest API too (same string its crawlers send).
var userAgent string

// newIngestClient never follows redirects: the bearer token goes to INGEST_URL and nowhere else, and a redirect is
// reported as an error instead.
func newIngestClient() *http.Client {
	return &http.Client{
		Timeout:       60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func httpSender(hc *http.Client, ingestURL, token string) sender {
	return func(path string, rows any) error { return post(hc, ingestURL+path, token, rows) }
}

func post(hc *http.Client, url, token string, rows any) error {
	body, err := json.Marshal(map[string]any{"rows": rows})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		se := &statusError{status: resp.StatusCode, body: strings.TrimSpace(string(b))}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			se.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
		}
		return se
	}
	return nil
}
