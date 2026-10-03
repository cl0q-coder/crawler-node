package main

// Watermark tests: when forward moves the stored position, when it dead-letters a batch and when it retries.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeStore serves rows with ids 1..n of one table and records watermark and dead-letter writes.
type fakeStore struct {
	n, mark   int64
	markErr   error
	writes    []int64
	dead      [][2]int64
	deadErr   error
	readCalls int
}

func (f *fakeStore) watermark(string) (int64, error) { return f.mark, f.markErr }
func (f *fakeStore) setWatermark(_ string, id int64) error {
	f.mark = id
	f.writes = append(f.writes, id)
	return nil
}
func (f *fakeStore) deadLetter(_ string, first, last int64, _ int, _ string, _ any) error {
	if f.deadErr != nil {
		return f.deadErr
	}
	f.dead = append(f.dead, [2]int64{first, last})
	return nil
}
func (f *fakeStore) readBatch(_ string, after int64, limit int) (any, int, int, int64, error) {
	f.readCalls++
	var rows []diRow
	for id := after + 1; id <= f.n && len(rows) < limit; id++ {
		rows = append(rows, diRow{Domain: "d.example"})
	}
	return rows, len(rows), len(rows), after + int64(len(rows)), nil
}

func sendWith(status int) sender {
	return func(string, any) error {
		if status == 200 {
			return nil
		}
		return &statusError{status: status}
	}
}

func TestForwardAdvancesAfterSuccess(t *testing.T) {
	st := &fakeStore{n: 25}
	if got := forward(context.Background(), st, sendWith(200), "domain_info", "/p", 10, false); got != 25 {
		t.Fatalf("forwarded %d, want 25", got)
	}
	if st.mark != 25 || len(st.writes) != 3 {
		t.Fatalf("watermark %d after %d writes, want 25 after 3 batches", st.mark, len(st.writes))
	}
}

func TestForwardRetriesTransientErrors(t *testing.T) {
	for _, status := range []int{401, 403, 408, 429, 500, 502, 503} {
		st := &fakeStore{n: 5}
		forward(context.Background(), st, sendWith(status), "domain_info", "/p", 10, false)
		if st.mark != 0 || len(st.dead) != 0 {
			t.Errorf("status %d: watermark %d, dead-lettered %v — a retryable answer must not move anything", status, st.mark, st.dead)
		}
	}
	st := &fakeStore{n: 5}
	forward(context.Background(), st, func(string, any) error { return errors.New("dial tcp: connection refused") }, "domain_info", "/p", 10, false)
	if st.mark != 0 {
		t.Error("a network error must not move the watermark")
	}
}

// A batch the ingest rejects for good is dead-lettered and skipped, so the rows behind it still go out.
func TestForwardDeadLettersPermanentRejections(t *testing.T) {
	st := &fakeStore{n: 25}
	calls := 0
	send := func(string, any) error {
		calls++
		if calls == 2 {
			return &statusError{status: 400, body: "bad JSON"}
		}
		return nil
	}
	if got := forward(context.Background(), st, send, "domain_info", "/p", 10, false); got != 15 {
		t.Fatalf("forwarded %d, want 15 (the rejected batch of 10 is not counted)", got)
	}
	if st.mark != 25 || len(st.dead) != 1 || st.dead[0] != [2]int64{11, 20} {
		t.Fatalf("watermark %d, dead-letter %v; want 25 and ids 11–20", st.mark, st.dead)
	}
	st = &fakeStore{n: 5, deadErr: errors.New("disk full")}
	forward(context.Background(), st, sendWith(413), "domain_info", "/p", 10, false)
	if st.mark != 0 {
		t.Error("if the dead-letter write fails the batch must be retried, not skipped")
	}
}

func TestForwardDryRunNeverWrites(t *testing.T) {
	st := &fakeStore{n: 25}
	forward(context.Background(), st, func(string, any) error { t.Fatal("dry run must not POST"); return nil }, "domain_info", "/p", 10, true)
	if len(st.writes) != 0 {
		t.Fatalf("dry run wrote the watermark %v", st.writes)
	}
}

// A failed watermark read must not restart from 0 (that would re-send — and re-upsert — the whole table).
func TestForwardSkipsCycleOnWatermarkError(t *testing.T) {
	st := &fakeStore{n: 25, markErr: errors.New("connection reset")}
	if got := forward(context.Background(), st, sendWith(200), "domain_info", "/p", 10, false); got != 0 || st.readCalls != 0 {
		t.Fatalf("forwarded %d with %d reads after a watermark error, want nothing", got, st.readCalls)
	}
}

func TestForwardStopsOnShutdown(t *testing.T) {
	st := &fakeStore{n: 1000}
	ctx, cancel := context.WithCancel(context.Background())
	send := func(string, any) error { cancel(); return nil } // shutdown arrives during the first POST
	forward(ctx, st, send, "domain_info", "/p", 10, false)
	if st.mark != 10 {
		t.Fatalf("watermark %d; the batch in flight must be recorded and nothing more sent", st.mark)
	}
}

func TestCheckIngestURL(t *testing.T) {
	if u, err := checkIngestURL("https://cl0q.com/", false); err != nil || u != "https://cl0q.com" {
		t.Errorf("https URL: %q, %v", u, err)
	}
	for _, bad := range []string{"http://cl0q.com", "cl0q.com", "ftp://cl0q.com", ""} {
		if _, err := checkIngestURL(bad, false); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	if _, err := checkIngestURL("http://127.0.0.1:8091", true); err != nil {
		t.Errorf("http is allowed with CL0Q_NODE_ALLOW_HTTP=1: %v", err)
	}
}

// The token-carrying client never follows a redirect; the 3xx comes back as an error.
func TestIngestClientDoesNotFollowRedirects(t *testing.T) {
	hit := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	err := post(newIngestClient(), srv.URL+"/api/ingest/domains", "secret", []diRow{{Domain: "d.example"}})
	var se *statusError
	if !errors.As(err, &se) || se.status != http.StatusTemporaryRedirect || hit {
		t.Fatalf("got %v (followed=%v); want a 307 error and no request to the redirect target", err, hit)
	}
	if _, dead := permanent(err); dead {
		t.Error("a 3xx is not a permanent rejection")
	}
}

func TestSendable(t *testing.T) {
	for fe, want := range map[string]bool{"": true, "GET failed: EOF": true, "challenge:cloudflare": false,
		"blocked:non-public-address": false, "robots_disallow": true} {
		if sendable(fe) != want {
			t.Errorf("sendable(%q) = %v", fe, !want)
		}
	}
}
