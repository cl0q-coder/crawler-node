package main

// NR7: never-lose-a-domain. Row conservation across the forwarder: every
// row read from the local tables must end up in exactly one of three
// states — sent to the ingest, dead-lettered with a reason, or still
// pending (watermark not advanced past it). No row may be lost, none may
// be counted twice.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// idStore is a fake store whose rows carry their ids in the Domain field
// ("d<id>.example") so the test sender can track exactly which rows it saw.
type idStore struct {
	n, mark int64
	dead    [][2]int64
}

func (s *idStore) watermark(string) (int64, error) { return s.mark, nil }
func (s *idStore) setWatermark(_ string, id int64) error {
	s.mark = id
	return nil
}
func (s *idStore) deadLetter(_ string, first, last int64, _ int, _ string, _ any) error {
	s.dead = append(s.dead, [2]int64{first, last})
	return nil
}
func (s *idStore) readBatch(_ string, after int64, limit int) (any, int, int, int64, error) {
	var rows []diRow
	for id := after + 1; id <= s.n && len(rows) < limit; id++ {
		rows = append(rows, diRow{Domain: fmt.Sprintf("d%d.example", id)})
	}
	return rows, len(rows), len(rows), after + int64(len(rows)), nil
}

// rowIDs extracts the ids the sender was given.
func rowIDs(rows any) []int64 {
	var ids []int64
	for _, r := range rows.([]diRow) {
		id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(r.Domain, "d"), ".example"), 10, 64)
		ids = append(ids, id)
	}
	return ids
}

// TestForwardRowConservationMixedOutcomes: success, transient failure, and
// permanent rejection across cycles — every row accounted for exactly once.
func TestForwardRowConservationMixedOutcomes(t *testing.T) {
	st := &idStore{n: 30}
	var sent []int64
	call := 0
	send := func(_ string, rows any) error {
		call++
		ids := rowIDs(rows)
		switch call {
		case 1:
			sent = append(sent, ids...) // ids 1–10: success
			return nil
		case 2:
			return &statusError{status: 500} // ids 11–20: transient, stays pending
		case 3:
			sent = append(sent, ids...) // ids 11–20: success on retry
			return nil
		case 4:
			return &statusError{status: 400, body: "bad row"} // ids 21–30: rejected for good
		}
		return nil
	}

	// Cycle 1: batch 1 sent, batch 2 transient-fails → forward stops.
	if got := forward(context.Background(), st, send, "domain_info", "/p", 10, false); got != 10 {
		t.Fatalf("cycle 1 forwarded %d, want 10", got)
	}
	if st.mark != 10 {
		t.Fatalf("cycle 1 watermark = %d, want 10 (failed batch stays pending)", st.mark)
	}
	if len(st.dead) != 0 {
		t.Fatalf("cycle 1 dead-lettered %v, want none (transient is retried, not buried)", st.dead)
	}

	// Cycle 2: batch 2 retried OK, batch 3 permanently rejected → dead-lettered.
	if got := forward(context.Background(), st, send, "domain_info", "/p", 10, false); got != 10 {
		t.Fatalf("cycle 2 forwarded %d, want 10 (rejected batch not counted)", got)
	}
	if st.mark != 30 {
		t.Fatalf("cycle 2 watermark = %d, want 30", st.mark)
	}
	if len(st.dead) != 1 || st.dead[0] != [2]int64{21, 30} {
		t.Fatalf("dead-lettered %v, want [[21 30]]", st.dead)
	}

	// Conservation: sent + dead-lettered + pending == N, no overlaps.
	seen := map[int64]string{}
	for _, id := range sent {
		if prev, ok := seen[id]; ok {
			t.Fatalf("row %d counted twice (sent, previously %s)", id, prev)
		}
		seen[id] = "sent"
	}
	for _, r := range st.dead {
		for id := r[0]; id <= r[1]; id++ {
			if prev, ok := seen[id]; ok {
				t.Fatalf("row %d counted twice (dead-lettered, previously %s)", id, prev)
			}
			seen[id] = "dead"
		}
	}
	for id := st.mark + 1; id <= st.n; id++ {
		if prev, ok := seen[id]; ok {
			t.Fatalf("row %d counted twice (pending, previously %s)", id, prev)
		}
		seen[id] = "pending"
	}
	if len(seen) != int(st.n) {
		t.Fatalf("accounted for %d of %d rows — %d rows lost", len(seen), st.n, st.n-int64(len(seen)))
	}
	if len(sent) != 20 {
		t.Errorf("sent %d rows, want 20", len(sent))
	}
}

// TestForwardAuthFailureKeepsRowsPending: a 401/403 must not move the
// watermark or dead-letter — the rows wait for the re-auth cycle (NR5).
func TestForwardAuthFailureKeepsRowsPending(t *testing.T) {
	for _, status := range []int{401, 403} {
		st := &idStore{n: 10}
		var sent []int64
		send := func(_ string, rows any) error {
			sent = append(sent, rowIDs(rows)...)
			return &statusError{status: status}
		}
		forward(context.Background(), st, send, "domain_info", "/p", 10, false)
		if st.mark != 0 {
			t.Errorf("status %d: watermark moved to %d — auth-failed rows must stay pending", status, st.mark)
		}
		if len(st.dead) != 0 {
			t.Errorf("status %d: dead-lettered %v — auth failures are retried, not buried", status, st.dead)
		}
		if len(sent) != 10 {
			t.Errorf("status %d: sender saw %d rows, want 10 (the batch was attempted)", status, len(sent))
		}
	}
}

// TestForwardNeverAdvancesPastUndeadLettered: if the dead-letter write
// itself fails, the watermark must not advance — the batch is retried,
// never skipped.
func TestForwardNeverAdvancesPastUndeadLettered(t *testing.T) {
	st := &failingDeadStore{&idStore{n: 10}}
	forward(context.Background(), st, sendWith(400), "domain_info", "/p", 10, false)
	if st.mark != 0 {
		t.Errorf("watermark = %d, want 0 — a batch that couldn't be dead-lettered must be retried", st.mark)
	}
}

type failingDeadStore struct{ *idStore }

func (s *failingDeadStore) deadLetter(string, int64, int64, int, string, any) error {
	return fmt.Errorf("disk full")
}
