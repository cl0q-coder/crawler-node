package main

// HE-10 (conditional fetch optimization) tests: conditional headers are sent when
// validators are stored, 304s short-circuit the download, rotated validators on a 304
// are captured, and the bytes-saved accounting is correct. Plus a synthetic-fleet
// simulation that measures conditional coverage, 304 hit rate and bytes served
// per 1k recrawls before/after conditional fetch.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cl0q/internal/tunable"
	"cl0q/netguard"
)

func init() {
	if tunables == nil {
		tunables = tunable.New(time.Second, time.Second, 50)
	}
}

// he10Origin is a fake origin with configurable validator behavior for the HE-10 tests.
type he10Origin struct {
	srv *httptest.Server
	// behavior: "static" (validators + 304 on match), "changing" (validators, always 200),
	// "novalidator" (never sends validators).
	behavior string
	etag     string
	lastMod  string
	body     string

	mu            sync.Mutex
	seenNoneMatch []string // If-None-Match values seen, in request order
	seenModSince  []string
	bytesServed   atomic.Int64
}

func newHE10Origin(t *testing.T, behavior string) *he10Origin {
	t.Helper()
	o := &he10Origin{
		behavior: behavior,
		etag:     `"he10-v1"`,
		lastMod:  "Wed, 07 Oct 2026 12:00:00 GMT",
		body:     "<html><head><title>he10</title></head><body>" + strings.Repeat("x", 20000) + "</body></html>",
	}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.seenNoneMatch = append(o.seenNoneMatch, r.Header.Get("If-None-Match"))
		o.seenModSince = append(o.seenModSince, r.Header.Get("If-Modified-Since"))
		etag, lastMod := o.etag, o.lastMod
		o.mu.Unlock()
		if behavior == "static" && (r.Header.Get("If-None-Match") == etag || r.Header.Get("If-Modified-Since") == lastMod) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if behavior != "novalidator" {
			w.Header().Set("ETag", etag)
			w.Header().Set("Last-Modified", lastMod)
		}
		if behavior == "changing" {
			// content changes every fetch → new validator each time
			o.mu.Lock()
			o.etag = fmt.Sprintf(`"he10-v%d"`, time.Now().UnixNano())
			o.mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		n, _ := w.Write([]byte(o.body))
		o.bytesServed.Add(int64(n))
	}))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *he10Origin) host() string { return strings.TrimPrefix(o.srv.URL, "http://") }

func he10Ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 20*time.Second)
}

// resetHE10Counters gives the HE-10 counters fresh instances for measurement.
func resetHE10Counters() {
	mFetchAttempts = &atomic.Int64{}
	mConditionalReqs = &atomic.Int64{}
	mBytesSaved = &atomic.Int64{}
}

// A recrawl with stored validators must send If-None-Match/If-Modified-Since, and an
// unchanged origin must answer 304 → NotModified, skipping the body.
func TestHE10ConditionalHeadersAnd304(t *testing.T) {
	netguard.SetAllowNonPublicForTest(true)
	resetHE10Counters()
	o := newHE10Origin(t, "static")

	ctx, cancel := he10Ctx()
	defer cancel()
	info, err := fetchWebsiteInfo(ctx, o.host(), "", `"he10-v1"`, "Wed, 07 Oct 2026 12:00:00 GMT", nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !info.NotModified {
		t.Fatalf("want NotModified on matching validators, got status %d", info.StatusCode)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	last := len(o.seenNoneMatch) - 1 // first request is the https:// probe; the http:// retry carries the headers
	if o.seenNoneMatch[last] != `"he10-v1"` {
		t.Errorf("If-None-Match = %q, want %q", o.seenNoneMatch[last], `"he10-v1"`)
	}
	if o.seenModSince[last] != "Wed, 07 Oct 2026 12:00:00 GMT" {
		t.Errorf("If-Modified-Since = %q, want lastMod", o.seenModSince[last])
	}
	if got := mFetchAttempts.Load(); got < 1 {
		t.Errorf("fetch_attempts = %d, want >= 1", got)
	}
	if got := mConditionalReqs.Load(); got < 1 {
		t.Errorf("conditional_requests = %d, want >= 1", got)
	}
}

// Without stored validators the fetch must NOT send conditional headers.
func TestHE10NoValidatorsNoConditionalHeaders(t *testing.T) {
	netguard.SetAllowNonPublicForTest(true)
	resetHE10Counters()
	o := newHE10Origin(t, "static")

	ctx, cancel := he10Ctx()
	defer cancel()
	info, err := fetchWebsiteInfo(ctx, o.host(), "", "", "", nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if info.NotModified {
		t.Fatal("no validators sent → must not get a 304")
	}
	if info.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", info.StatusCode)
	}
	if info.ETag != `"he10-v1"` {
		t.Errorf("captured ETag = %q, want %q", info.ETag, `"he10-v1"`)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for i, v := range o.seenNoneMatch {
		if v != "" {
			t.Errorf("request %d sent If-None-Match %q with no stored validator", i, v)
		}
	}
	if got := mConditionalReqs.Load(); got != 0 {
		t.Errorf("conditional_requests = %d, want 0", got)
	}
	if got := mFetchAttempts.Load(); got < 1 {
		t.Errorf("fetch_attempts = %d, want >= 1", got)
	}
}

// A 304 may carry rotated validators (RFC 7232 §4.1) — they must be captured so the
// flush touch can refresh the stored copy.
func TestHE10304CapturesRotatedValidators(t *testing.T) {
	netguard.SetAllowNonPublicForTest(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"old"` {
			w.Header().Set("ETag", `"rotated-v2"`)
			w.Header().Set("Last-Modified", "Wed, 07 Oct 2026 13:00:00 GMT")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := he10Ctx()
	defer cancel()
	info, err := fetchWebsiteInfo(ctx, strings.TrimPrefix(srv.URL, "http://"), "", `"old"`, "", nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !info.NotModified {
		t.Fatal("want NotModified")
	}
	if info.ETag != `"rotated-v2"` {
		t.Errorf("304 ETag = %q, want rotated value", info.ETag)
	}
	if info.LastModified != "Wed, 07 Oct 2026 13:00:00 GMT" {
		t.Errorf("304 Last-Modified = %q, want rotated value", info.LastModified)
	}
}

// savedBytes: only a 304 with a known prior body size saves bytes.
func TestHE10SavedBytes(t *testing.T) {
	tgt := target{domain: "example.com", contentLen: 20480}
	if got := savedBytes(tgt, &WebsiteInfo{NotModified: true}); got != 20480 {
		t.Errorf("304 with contentLen → %d, want 20480", got)
	}
	if got := savedBytes(tgt, &WebsiteInfo{StatusCode: 200}); got != 0 {
		t.Errorf("200 → %d, want 0", got)
	}
	if got := savedBytes(target{domain: "example.com"}, &WebsiteInfo{NotModified: true}); got != 0 {
		t.Errorf("304 without contentLen → %d, want 0", got)
	}
	if got := savedBytes(tgt, nil); got != 0 {
		t.Errorf("nil info → %d, want 0", got)
	}
}

// TestHE10FleetSimulation runs a synthetic fleet of origins through two recrawl rounds:
// BEFORE (no stored validators → every fetch is full) and AFTER (stored validators from
// round one → conditional). It measures conditional coverage, 304 hit rate and bytes
// served per 1k recrawls on the real fetch path.
func TestHE10FleetSimulation(t *testing.T) {
	netguard.SetAllowNonPublicForTest(true)

	const nStatic, nChanging, nNoValidator = 40, 40, 20 // 100 origins
	var origins []*he10Origin
	for i := 0; i < nStatic; i++ {
		origins = append(origins, newHE10Origin(t, "static"))
	}
	for i := 0; i < nChanging; i++ {
		origins = append(origins, newHE10Origin(t, "changing"))
	}
	for i := 0; i < nNoValidator; i++ {
		origins = append(origins, newHE10Origin(t, "novalidator"))
	}

	fetchAll := func(t *testing.T, validators map[*he10Origin][2]string) (notmod int) {
		t.Helper()
		for _, o := range origins {
			ctx, cancel := he10Ctx()
			v := validators[o]
			info, err := fetchWebsiteInfo(ctx, o.host(), "", v[0], v[1], nil)
			cancel()
			if err != nil {
				t.Fatalf("fetch %s: %v", o.behavior, err)
			}
			if info.NotModified {
				notmod++
			}
		}
		return notmod
	}
	totalBytes := func() int64 {
		var n int64
		for _, o := range origins {
			n += o.bytesServed.Load()
		}
		return n
	}

	// BEFORE: cold recrawl, no validators stored → 0% conditional coverage.
	resetHE10Counters()
	validators := map[*he10Origin][2]string{}
	fetchAll(t, validators)
	beforeBytes := totalBytes()
	beforeAttempts := mFetchAttempts.Load()
	beforeConditional := mConditionalReqs.Load()

	// Harvest the validators a real first crawl would have stored (200s that sent them).
	stored := map[*he10Origin][2]string{}
	for _, o := range origins {
		ctx, cancel := he10Ctx()
		info, err := fetchWebsiteInfo(ctx, o.host(), "", "", "", nil)
		cancel()
		if err != nil {
			t.Fatalf("harvest %s: %v", o.behavior, err)
		}
		if info.StatusCode == http.StatusOK && (info.ETag != "" || info.LastModified != "") {
			stored[o] = [2]string{info.ETag, info.LastModified}
		}
	}

	// AFTER: recrawl with stored validators → conditional.
	resetHE10Counters()
	for _, o := range origins {
		o.bytesServed.Store(0)
	}
	notmod := fetchAll(t, stored)
	afterBytes := totalBytes()
	afterAttempts := mFetchAttempts.Load()
	afterConditional := mConditionalReqs.Load()

	coverage := float64(afterConditional) / float64(afterAttempts)
	hitRate := float64(notmod) / float64(afterConditional)
	per1kBefore := float64(beforeBytes) / float64(beforeAttempts) * 1000
	per1kAfter := float64(afterBytes) / float64(afterAttempts) * 1000

	t.Logf("HE-10 fleet sim (100 origins: 40 static + 40 changing + 20 no-validator):")
	t.Logf("  conditional coverage: %.1f%% (%d/%d)", coverage*100, afterConditional, afterAttempts)
	t.Logf("  304 hit rate:         %.1f%% (%d 304s / %d conditional)", hitRate*100, notmod, afterConditional)
	t.Logf("  bytes/1k recrawls:    before=%.0f after=%.0f (saved %.0f%%)",
		per1kBefore, per1kAfter, (1-per1kAfter/per1kBefore)*100)

	if beforeConditional != 0 {
		t.Errorf("before: conditional_requests = %d, want 0 (cold, no validators)", beforeConditional)
	}
	if coverage < 0.75 || coverage > 0.85 {
		t.Errorf("coverage = %.2f, want ~0.80 (80 validator origins / 100)", coverage)
	}
	if hitRate < 0.45 || hitRate > 0.55 {
		t.Errorf("304 hit rate = %.2f, want ~0.50 (40 static / 80 conditional)", hitRate)
	}
	if notmod != nStatic {
		t.Errorf("304s = %d, want %d (the static origins)", notmod, nStatic)
	}
	if per1kAfter >= per1kBefore {
		t.Errorf("after bytes/1k (%f) must be < before (%f)", per1kAfter, per1kBefore)
	}
	// 40 static origins × ~20KB bodies skipped → ≥50% of validator-origin bytes saved.
	if saved := 1 - per1kAfter/per1kBefore; saved < 0.35 {
		t.Errorf("bytes saved = %.1f%%, want >= 35%%", saved*100)
	}
}
