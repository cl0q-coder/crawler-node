package quality

// K5 (blacklist expansion): pluggable read-only threat-feed loader.
//
// The crawler already deletes pages matching the admin-curated cl0q_blacklist
// table (see cmd/website-crawler's loadBlacklist/isBlacklisted: `domain` kind =
// exact apex + subdomains via blDomainHit, `keyword` kind = dot/hyphen/space
// label-boundary match via blWordBoundary — the SafeSearch lesson: never eat
// legit sites on a substring).
//
// This file adds the FEED side of K5: reputable read-only malware/phishing
// feeds (URLhaus-style, OpenPhish-style) fetched on a refresh timer and
// normalized into the SAME domain-kind matching the existing matcher already
// does. Design rules, per Claude's K5 GO (QA doc 2026-10-16):
//   - Feeds are READ-ONLY: plain HTTP GET, no credentials, never POSTs back.
//   - Feed entries become DOMAIN-kind rules only (exact apex + subdomains).
//     They are NEVER keyword rules: a feed URL fragment must not become a
//     substring that eats legit neighbors (short-keyword collateral rule).
//   - A failed fetch/parse KEEPS the previously-loaded set (same partial-load
//     guard as loadBlacklist): a flaky feed must fail closed to the last good
//     snapshot, never to an empty blocklist.
//   - Every installed host carries its feed name as provenance, so the
//     labeled-sample precision measurement can attribute hits per feed before
//     anything gates on them (the pre-index gate is a separate PR; Claude RED).
//   - Zero feeds configured = today's behavior exactly (disabled by default).

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Feed is one read-only threat feed: a named source of malicious host
// indicators. Implementations MUST be read-only (plain GET, no auth, no
// writeback); the Refresher calls Fetch on its timer.
type Feed interface {
	// Name identifies the feed in logs, metrics, and match provenance
	// (e.g. "urlhaus").
	Name() string
	// Fetch returns the feed's raw lines. A non-nil error keeps the
	// previously-loaded set installed.
	Fetch(ctx context.Context) ([]string, error)
}

// URLFeed fetches a newline-delimited text feed: one URL or bare host per
// line, '#' starts a comment, blank lines skipped.
type URLFeed struct {
	name     string
	feedURL  string
	client   *http.Client
	maxBytes int64
}

// URLFeed options.
const (
	defaultFeedTimeout = 60 * time.Second
	defaultFeedMaxByte = 32 << 20 // 32 MiB cap per fetch
)

// NewURLFeed builds a read-only HTTP feed loader.
func NewURLFeed(name, feedURL string) *URLFeed {
	return &URLFeed{
		name:    name,
		feedURL: feedURL,
		client: &http.Client{
			Timeout: defaultFeedTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return fmt.Errorf("feed %s: too many redirects", name)
				}
				return nil
			},
		},
		maxBytes: defaultFeedMaxByte,
	}
}

func (f *URLFeed) Name() string { return f.name }

// Fetch performs the single allowed network operation: one HTTP GET.
func (f *URLFeed) Fetch(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.feedURL, nil)
	if err != nil {
		return nil, fmt.Errorf("feed %s: %w", f.name, err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("feed %s: %w", f.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feed %s: HTTP %d", f.name, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes))
	if err != nil {
		return nil, fmt.Errorf("feed %s: %w", f.name, err)
	}
	var lines []string
	for _, ln := range strings.Split(string(body), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		lines = append(lines, ln)
	}
	return lines, nil
}

// NormalizeHost reduces one raw feed line to the canonical host it blocks,
// or ok=false when the line carries nothing usable. Rules:
//   - Full URLs reduce to their host (feed lines are usually URLs).
//   - Lowercase, strip port, trailing dot, and a single leading "www.".
//   - Reject: empty, IP literals are KEPT (exact-host match only —
//     blDomainHit walks labels, so "1.2.3.4" matches only that literal host),
//     hosts with whitespace/control chars, hosts without a dot that are not
//     "localhost"-style single labels we explicitly keep out.
//   - Internationalized names: keep the A-label (punycode) form when the
//     line already carries it; do NOT do IDNA mapping here — ShardOf parity
//     work (#301) showed fold does no IDNA normalization, and silently
//     mapping forms would split one logical host across two rules.
func NormalizeHost(raw string) (host string, ok bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	// Full URL -> host.
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return "", false
		}
		s = u.Host
	}
	// Strip any path/query/fragment that survived without a scheme.
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	// Strip port (careful with IPv6 literals).
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, ".")
	s = strings.TrimPrefix(s, "www.")
	if s == "" {
		return "", false
	}
	for _, r := range s {
		if r <= 0x20 || r == 0x7f {
			return "", false
		}
	}
	// Must look like a host: letters/digits/dots/hyphens (or an IP literal).
	if net.ParseIP(s) == nil {
		if !strings.Contains(s, ".") {
			return "", false
		}
		for _, r := range s {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r >= 0x80) {
				return "", false
			}
		}
		for _, lbl := range strings.Split(s, ".") {
			if lbl == "" || len(lbl) > 63 {
				return "", false
			}
		}
	}
	return s, true
}

// feedResult is one feed's normalized contribution.
type feedResult struct {
	name    string
	domains map[string]bool // normalized host -> true
}

// Refresher owns the K5 feed set: it refreshes every feed on a timer (with
// jitter so N nodes don't thunder the feed host), keeps the last good
// snapshot on any failure, and serves an atomic snapshot for the matcher.
type Refresher struct {
	mu       sync.RWMutex
	feeds    []Feed
	interval time.Duration
	domains  map[string]string // normalized host -> feed name (provenance)
	lastOK   map[string]time.Time
	lastErr  map[string]error
}

// NewRefresher builds the refresher. An empty feeds slice is legal and means
// "disabled": Domains() then returns an empty snapshot and Start() is a no-op
// ticker that never fires network I/O.
func NewRefresher(feeds []Feed, interval time.Duration) *Refresher {
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	return &Refresher{
		feeds:    feeds,
		interval: interval,
		domains:  map[string]string{},
		lastOK:   map[string]time.Time{},
		lastErr:  map[string]error{},
	}
}

// Refresh performs one fetch+normalize pass over every feed. Feeds that fail
// keep their previously-loaded hosts; only fully-parsed feeds replace their
// contribution. It returns the first error encountered (if any) but still
// installs every feed that succeeded.
func (r *Refresher) Refresh(ctx context.Context) error {
	var firstErr error
	next := map[string]string{}
	r.mu.RLock()
	prev := r.domains
	r.mu.RUnlock()
	for _, f := range r.feeds {
		lines, err := f.Fetch(ctx)
		if err != nil {
			r.mu.Lock()
			r.lastErr[f.Name()] = err
			// Keep this feed's previous hosts.
			for h, prov := range prev {
				if prov == f.Name() {
					next[h] = prov
				}
			}
			r.mu.Unlock()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		seen := map[string]bool{}
		for _, ln := range lines {
			h, ok := NormalizeHost(ln)
			if !ok || seen[h] {
				continue
			}
			seen[h] = true
			next[h] = f.Name()
		}
		r.mu.Lock()
		r.lastOK[f.Name()] = time.Now()
		delete(r.lastErr, f.Name())
		r.mu.Unlock()
	}
	r.mu.Lock()
	r.domains = next
	r.mu.Unlock()
	return firstErr
}

// Start runs the refresh loop until ctx ends: one immediate refresh, then the
// interval ticker with +/-10% jitter. A nil/empty feed set still runs (cheap
// no-op ticks) so wiring stays uniform.
func (r *Refresher) Start(ctx context.Context) {
	_ = r.Refresh(ctx) // best-effort initial load; failures keep empty set
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Jitter the fetch so a fleet doesn't synchronize on feed hosts.
			j := time.Duration((rand.Float64()*0.2 - 0.1) * float64(r.interval))
			select {
			case <-ctx.Done():
				return
			case <-time.After(j):
			}
			_ = r.Refresh(ctx)
		}
	}
}

// Domains returns an atomic snapshot: normalized host -> feed name. The
// caller merges it into its own matcher (domain-kind only).
func (r *Refresher) Domains() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.domains))
	for h, prov := range r.domains {
		out[h] = prov
	}
	return out
}

// Count reports installed hosts (for metrics).
func (r *Refresher) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.domains)
}

// FeedStatus reports per-feed health for logging/metrics.
func (r *Refresher) FeedStatus() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string]string{}
	for _, f := range r.feeds {
		if err, bad := r.lastErr[f.Name()]; bad {
			out[f.Name()] = "error: " + err.Error()
		} else if t, ok := r.lastOK[f.Name()]; ok {
			out[f.Name()] = "ok @ " + t.Format(time.RFC3339)
		} else {
			out[f.Name()] = "never fetched"
		}
	}
	return out
}
