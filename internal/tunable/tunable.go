// Package tunable provides live-reloadable crawler settings (IG6).
// Operators edit a JSON control file; the crawler picks up changes without
// restart. Inspired by ArchiveTeam's grab-site control files.
//
// Control file format (JSON):
//
//	{
//	  "host_delay_ms": 1000,
//	  "ip_delay_ms": 1000,
//	  "max_concurrency": 50
//	}
//
// All fields optional; missing fields keep current values.
package tunable

import (
	"encoding/json"
	"os"
	"sync/atomic"
	"time"
)

// Config holds the tunable settings.
type Config struct {
	HostDelayMs    *int `json:"host_delay_ms,omitempty"`
	IPDelayMs      *int `json:"ip_delay_ms,omitempty"`
	MaxConcurrency *int `json:"max_concurrency,omitempty"`
}

// Tunables holds atomic values for live-updated settings.
type Tunables struct {
	hostDelayNs    atomic.Int64
	ipDelayNs      atomic.Int64
	maxConcurrency atomic.Int64
}

// New creates Tunables with initial values.
func New(hostDelay, ipDelay time.Duration, maxConcurrency int) *Tunables {
	t := &Tunables{}
	t.hostDelayNs.Store(int64(hostDelay))
	t.ipDelayNs.Store(int64(ipDelay))
	t.maxConcurrency.Store(int64(maxConcurrency))
	return t
}

// HostDelay returns the current per-host delay.
func (t *Tunables) HostDelay() time.Duration {
	return time.Duration(t.hostDelayNs.Load())
}

// IPDelay returns the current per-IP delay.
func (t *Tunables) IPDelay() time.Duration {
	return time.Duration(t.ipDelayNs.Load())
}

// MaxConcurrency returns the current max concurrency.
func (t *Tunables) MaxConcurrency() int {
	return int(t.maxConcurrency.Load())
}

// Update applies a Config, changing only the fields that are set.
// Returns true if any value changed.
func (t *Tunables) Update(c Config) bool {
	changed := false
	if c.HostDelayMs != nil {
		ns := int64(*c.HostDelayMs) * int64(time.Millisecond)
		// Enforce 1s floor (politeness minimum).
		if ns < int64(time.Second) {
			ns = int64(time.Second)
		}
		if t.hostDelayNs.Swap(ns) != ns {
			changed = true
		}
	}
	if c.IPDelayMs != nil {
		ns := int64(*c.IPDelayMs) * int64(time.Millisecond)
		if ns < int64(time.Second) {
			ns = int64(time.Second)
		}
		if t.ipDelayNs.Swap(ns) != ns {
			changed = true
		}
	}
	if c.MaxConcurrency != nil && *c.MaxConcurrency > 0 {
		if t.maxConcurrency.Swap(int64(*c.MaxConcurrency)) != int64(*c.MaxConcurrency) {
			changed = true
		}
	}
	return changed
}

// SetHostDelayForTest sets the host delay without the politeness floor.
// For tests only.
func (t *Tunables) SetHostDelayForTest(d time.Duration) {
	t.hostDelayNs.Store(int64(d))
}

// LoadFile reads and parses a control file.
func LoadFile(path string) (Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	return c, nil
}
