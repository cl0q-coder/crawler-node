// Package daily provides day-bucketed counters for node-runner dashboards (OP4).
//
// Counters roll over at UTC midnight. Keeps today + yesterday so "vs yesterday"
// comparisons work. Thread-safe.
package daily

import (
	"sync"
	"time"
)

// Stats holds one day's counters.
type Stats struct {
	Day            string // YYYY-MM-DD (UTC)
	DomainsCrawled int64
	PagesFetched   int64
	Errors         int64
	BytesFetched   int64
}

// Tracker tracks today and yesterday's stats.
type Tracker struct {
	mu        sync.Mutex
	today     Stats
	yesterday Stats
}

// New creates a Tracker for the current UTC day.
func New() *Tracker {
	t := &Tracker{}
	t.today.Day = time.Now().UTC().Format("2006-01-02")
	return t
}

// rollover checks if the UTC day changed and rotates today -> yesterday.
func (t *Tracker) rollover() {
	now := time.Now().UTC().Format("2006-01-02")
	if t.today.Day != now {
		t.yesterday = t.today
		t.today = Stats{Day: now}
	}
}

// AddDomains records n domains crawled.
func (t *Tracker) AddDomains(n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollover()
	t.today.DomainsCrawled += n
}

// AddPages records n pages fetched with b bytes.
func (t *Tracker) AddPages(n, b int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollover()
	t.today.PagesFetched += n
	t.today.BytesFetched += b
}

// AddErrors records n errors.
func (t *Tracker) AddErrors(n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollover()
	t.today.Errors += n
}

// Today returns a copy of today's stats.
func (t *Tracker) Today() Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollover()
	return t.today
}

// Yesterday returns a copy of yesterday's stats.
func (t *Tracker) Yesterday() Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.yesterday
}

// Delta returns today minus yesterday for each counter.
func (t *Tracker) Delta() Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollover()
	return Stats{
		Day:            "delta",
		DomainsCrawled: t.today.DomainsCrawled - t.yesterday.DomainsCrawled,
		PagesFetched:   t.today.PagesFetched - t.yesterday.PagesFetched,
		Errors:         t.today.Errors - t.yesterday.Errors,
		BytesFetched:   t.today.BytesFetched - t.yesterday.BytesFetched,
	}
}
