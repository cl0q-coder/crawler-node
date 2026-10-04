// Package dbresil provides shared remote-DB outage resilience for the cl0q services:
// wait-for-remote backoff, a background health flag (so handlers can serve a maintenance message
// instead of timing out), connection-down classification, and a local-Postgres spool (outbox) for
// store-and-forward during an outage. A nil *Spooler means spooling is disabled (no SPOOL DSN), so
// callers transparently fall back to their existing behaviour.
package dbresil

import (
	"context"
	"database/sql"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/lib/pq" // registers the "postgres" driver for any importer (e.g. the drain binary)
)

// WithConnectTimeout appends connect_timeout=<secs> to a lib/pq DSN (URL or key=value form) unless it already sets
// one. lib/pq's driver has no OpenConnector, so database/sql dials via driver.Open and IGNORES any context deadline:
// without connect_timeout a dial into a partitioned DB host waits out the kernel SYN retries (~2m10s — 132s hangs
// measured 2026-09-24). Applied to every lib/pq service since 2026-09-28.
func WithConnectTimeout(dsn string, secs int) string {
	if dsn == "" || strings.Contains(dsn, "connect_timeout") {
		return dsn
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		return dsn + sep + "connect_timeout=" + strconv.Itoa(secs)
	}
	return strings.TrimSpace(dsn) + " connect_timeout=" + strconv.Itoa(secs)
}

// Reachable reports whether db answers a short ping.
func Reachable(db *sql.DB) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return db.PingContext(ctx) == nil
}

// WaitForRemote blocks until db answers a ping, with capped exponential backoff (2s→60s). Used
// instead of crashing when the remote is unreachable, so a service rides out an outage and resumes.
func WaitForRemote(db *sql.DB) { _ = WaitForRemoteCtx(context.Background(), db) }

// WaitForRemoteCtx is WaitForRemote that gives up when ctx ends (a shutdown signal), returning ctx.Err().
func WaitForRemoteCtx(ctx context.Context, db *sql.DB) error {
	delay := 2 * time.Second
	for {
		if Reachable(db) {
			return nil
		}
		log.Printf("[dbresil] remote DB unreachable — retrying in %s", delay)
		if !SleepCtx(ctx, delay) {
			return ctx.Err()
		}
		delay = min(delay*2, 60*time.Second)
	}
}

// SleepCtx sleeps for d or until ctx ends, and reports whether the full d elapsed.
func SleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// IsDown reports whether a non-nil query error is due to the remote being unreachable (→ spool),
// as opposed to a data/poison-row error (→ don't spool). It confirms with a short ping.
func IsDown(db *sql.DB, err error) bool {
	return err != nil && !Reachable(db)
}

// Health tracks remote up/down via a background pinger so read handlers can fail fast with a
// maintenance message instead of paying the per-request DB timeout during an outage.
type Health struct {
	up   atomic.Bool
	stop chan struct{}
	once sync.Once
}

// safeGo runs fn in a background goroutine that cannot crash the process: a
// panic (e.g. a malformed lib/pq frame from the health pinger) is logged and,
// after a short backoff, fn is restarted — mirroring the goSafe wrapper used
// by every other background goroutine in the services.
func safeGo(name string, fn func()) {
	go func() {
		for {
			ok := func() (returned bool) {
				defer func() {
					if rec := recover(); rec != nil {
						log.Printf("[dbresil] goroutine %q panicked: %v", name, rec)
					}
				}()
				fn()
				return true
			}()
			if ok {
				return
			}
			time.Sleep(2 * time.Second) // backoff before restarting a panicked task
		}
	}()
}

// NewHealth starts a background pinger that refreshes the up/down flag every `every` until Stop is called.
func NewHealth(db *sql.DB, every time.Duration) *Health {
	h := &Health{stop: make(chan struct{})}
	h.up.Store(Reachable(db))
	safeGo("dbresil-health", func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				h.up.Store(Reachable(db))
			case <-h.stop:
				return
			}
		}
	})
	return h
}

// Stop ends the background pinger. Safe to call more than once.
func (h *Health) Stop() { h.once.Do(func() { close(h.stop) }) }

// Up reports the last observed remote reachability.
func (h *Health) Up() bool { return h.up.Load() }

// MarkDown flips the flag to down immediately (call it on a connection-down error for instant
// fail-fast); the background pinger restores it once the remote recovers.
func (h *Health) MarkDown() { h.up.Store(false) }

// Spooler wraps a local "outbox" DB. nil-safe: a nil *Spooler means spooling is disabled, so every
// method is a no-op and callers keep their existing drop/retry behaviour.
type Spooler struct {
	db      *sql.DB
	maxRows int
}

// Open returns a Spooler for dsn, or nil if dsn is empty (spooling disabled). maxRows is the
// per-table disk cap so a long outage can't fill the box (0 = unlimited).
func Open(dsn string, maxRows int) *Spooler {
	if dsn == "" {
		return nil
	}
	db, err := sql.Open("postgres", WithConnectTimeout(dsn, 10))
	if err != nil {
		log.Printf("[dbresil] spool open failed: %v", err)
		return nil
	}
	db.SetMaxOpenConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	return &Spooler{db: db, maxRows: maxRows}
}

// DB returns the local outbox handle; callers reuse their own write SQL against it.
func (s *Spooler) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

// OverCap reports whether the local `table` already holds >= maxRows, so the caller drops new spool
// writes rather than filling the disk. `table` is a trusted constant from our own code. The count stops at
// maxRows, so it stays cheap on a big outbox. Best-effort: false on a nil spooler, maxRows<=0, or any error.
func (s *Spooler) OverCap(table string) bool {
	if s == nil || s.maxRows <= 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT 1 FROM "+table+" LIMIT $1) t", s.maxRows).Scan(&n); err != nil {
		return false
	}
	return n >= s.maxRows
}
