// Package footprint bounds the node's resource use (NR3): DB rows, disk,
// and the graceful "at cap" behavior. An operator should be able to run a
// node on a small VPS without watching it.
//
// When a cap is reached the crawler stops pulling NEW domains (discovery
// pauses) but keeps crawling and forwarding what's already there, and logs
// a clear "at cap" message (throttled). Nothing is deleted; when the
// operator raises the cap, discovery resumes.
package footprint

import (
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	"codeberg.org/cl0qsearch/crawler-node/internal/config"
)

// caps is the evaluated state from one size check.
type caps struct {
	domains, pages, dbBytes       int64
	domainsCap, pagesCap, diskCap bool
}

// evaluateCaps is the pure decision logic: which caps are hit given the
// current sizes. A zero max disables that cap.
func evaluateCaps(domains, pages, dbBytes, maxDomains, maxPages, maxDBBytes int64) caps {
	return caps{
		domains:    domains,
		pages:      pages,
		dbBytes:    dbBytes,
		domainsCap: maxDomains > 0 && domains >= maxDomains,
		pagesCap:   maxPages > 0 && pages >= maxPages,
		diskCap:    maxDBBytes > 0 && dbBytes >= maxDBBytes,
	}
}

// Checker tracks whether the node has hit a footprint cap. It checks the
// database at most once a minute; the "at cap" log fires at most once per
// 10 minutes so a capped node doesn't spam.
type Checker struct {
	db         *sql.DB
	maxDomains int64 // 0 = uncapped
	maxPages   int64 // 0 = uncapped
	maxDBBytes int64 // 0 = uncapped

	mu        sync.Mutex
	lastCheck time.Time
	lastLog   time.Time
	state     caps
}

// New reads the caps from the environment:
// NODE_MAX_DOMAINS (default 1M), NODE_MAX_PAGES (default 1M),
// NODE_MAX_DB_GB (default 20). Zero disables a cap.
func New(db *sql.DB) *Checker {
	return &Checker{
		db:         db,
		maxDomains: int64(config.GetInt("NODE_MAX_DOMAINS", 1000000)),
		maxPages:   int64(config.GetInt("NODE_MAX_PAGES", 1000000)),
		maxDBBytes: int64(config.GetInt("NODE_MAX_DB_GB", 20)) << 30,
	}
}

// refreshLocked re-reads the sizes if the cache is stale.
func (c *Checker) refreshLocked() {
	if time.Since(c.lastCheck) < time.Minute {
		return
	}
	c.lastCheck = time.Now()
	var domains, pages, dbBytes int64
	// reltuples estimates instead of full COUNT(*): these tables hold 13M+
	// rows and refreshLocked runs every minute on a 1-core DB, where even a
	// 195k-row read costs 2-4s. The caps are coarse thresholds, so planner
	// estimates are fine (they track vacuum/analyze within a few percent).
	err := c.db.QueryRow(`SELECT (SELECT reltuples::bigint FROM pg_class WHERE relname = 'domain_info'),
		(SELECT reltuples::bigint FROM pg_class WHERE relname = 'website_info'),
		pg_database_size(current_database())`).Scan(&domains, &pages, &dbBytes)
	if err != nil {
		// A failed size check must not pause the crawler; log and keep going.
		log.Printf("footprint: size check failed (%v) — continuing uncapped", err)
		return
	}
	c.state = evaluateCaps(domains, pages, dbBytes, c.maxDomains, c.maxPages, c.maxDBBytes)
}

// PauseDiscovery reports whether NEW domains should stop being added, and
// why. The crawler keeps processing what's already queued.
func (c *Checker) PauseDiscovery() (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshLocked()
	switch {
	case c.state.domainsCap:
		return true, c.atCapLogf("domain_info at cap (%d >= %d rows) — pausing discovery, still crawling/forwarding", c.state.domains, c.maxDomains)
	case c.state.diskCap:
		return true, c.atCapLogf("database at cap (%.1f GB >= %d GB) — pausing discovery, still crawling/forwarding", float64(c.state.dbBytes)/1e9, c.maxDBBytes>>30)
	}
	return false, ""
}

// PauseNewPages reports whether website_info inserts for NEW domains should
// stop (updates to existing rows are fine). The crawler keeps recrawling.
func (c *Checker) PauseNewPages() (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshLocked()
	if c.state.pagesCap {
		return true, c.atCapLogf("website_info at cap (%d >= %d rows) — new domains skipped, still recrawling/updating", c.state.pages, c.maxPages)
	}
	if c.state.diskCap {
		return true, c.atCapLogf("database at cap (%.1f GB >= %d GB) — new domains skipped, still recrawling/updating", float64(c.state.dbBytes)/1e9, c.maxDBBytes>>30)
	}
	return false, ""
}

// atCapLogf logs the "at cap" message at most once per 10 minutes and
// returns it for the caller.
func (c *Checker) atCapLogf(format string, args ...any) string {
	msg := "footprint: " + fmt.Sprintf(format, args...)
	if time.Since(c.lastLog) >= 10*time.Minute {
		log.Print(msg)
		c.lastLog = time.Now()
	}
	return msg
}
