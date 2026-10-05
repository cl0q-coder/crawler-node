// Auth hardening (NR5): expired/revoked token handling for the ingest API.
//
// The ingest is a public boundary, so token failures get explicit,
// non-spinning handling:
//
//   - 401 (expired/invalid token): try automatic re-enrol (idempotent
//     rotation) when the node enrolled via `cl0q-node enroll` (we have a
//     node_id to rotate). The batch is retried once with the fresh token.
//   - 401 with a NODE_TOKEN env var (no credential file): can't rotate
//     automatically — log exactly what to do and back off.
//   - 403 + {"revoked": true}: the token is dead. Wipe it locally, tell
//     the operator to re-enrol, and back off hard. Never spin on it.
//   - 403 without the revoked flag: likely a source-IP mismatch — say so.
//
// Nothing is lost during re-auth: the watermark only advances after a
// successful POST, so failed batches are retried. Backoff is exponential
// (1m, 2m, 4m … capped at 1h) so a dead token never becomes a tight loop.
package main

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"sync"
	"time"
)

// tokenSource holds the current ingest credentials. The token is mutable:
// rotation swaps it without restarting the forwarder.
type tokenSource struct {
	mu        sync.Mutex
	token     string
	nodeID    string
	credPath  string // "" when the token came from env (no auto-rotation)
	ingestURL string

	authFails int       // consecutive auth failures (for backoff)
	backoff   time.Time // don't attempt sends before this (auth backoff)
}

func (ts *tokenSource) get() string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.token
}

// authBackoff reports how long the main loop should wait before the next
// cycle after auth failures. Zero means "use the normal interval".
func (ts *tokenSource) authBackoff() time.Duration {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.authFails == 0 {
		return 0
	}
	wait := time.Until(ts.backoff)
	if wait < 0 {
		wait = 0
	}
	return wait
}

// isRevoked reports whether an ingest error body carries the explicit
// revocation flag (PROTOCOL.md §3).
func isRevoked(body string) bool {
	var v struct {
		Revoked bool `json:"revoked"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return false
	}
	return v.Revoked
}

// authBackoffFor returns the backoff after n consecutive auth failures:
// 1m, 2m, 4m, … capped at 1h.
func authBackoffFor(n int) time.Duration {
	d := time.Minute << n
	if d > time.Hour || d <= 0 {
		return time.Hour
	}
	return d
}

// handleAuthFailure processes a 401/403 from the ingest. It returns true if
// the caller should retry the batch immediately (we re-enrolled), false if
// it should back off. It never logs the token.
func (ts *tokenSource) handleAuthFailure(status int, body string) (retry bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	fail := func(msg string) bool {
		ts.authFails++
		ts.backoff = time.Now().Add(authBackoffFor(ts.authFails))
		log.Printf("ingest auth: %s (backing off %s)", msg, authBackoffFor(ts.authFails))
		return false
	}

	if status == 403 && isRevoked(body) {
		// The token is dead. Wipe it so nothing keeps trying it, keep the
		// node_id so `cl0q-node enroll` can rotate in place.
		ts.token = ""
		if ts.credPath != "" {
			if err := writeCredentials(ts.credPath, ts.nodeID, ""); err != nil {
				log.Printf("ingest auth: failed to wipe revoked token: %v", err)
			}
		}
		ts.authFails++
		ts.backoff = time.Now().Add(time.Hour)
		log.Printf("ingest auth: token REVOKED by cl0q — local credentials wiped. " +
			"Run `cl0q-node enroll` to get a new token. Backing off 1h.")
		return false
	}

	if status == 403 {
		return fail("HTTP 403 without revocation flag — likely a source-IP mismatch (token bound to the enrolment IP). Check the node is crawling from its enrolled address")
	}

	// 401: expired or invalid token. Try automatic rotation if we can.
	if ts.credPath == "" {
		return fail("HTTP 401 (token expired/invalid) — token came from NODE_TOKEN env, cannot rotate automatically. Update NODE_TOKEN or run `cl0q-node enroll`, then restart")
	}
	ts.mu.Unlock()
	err := doEnroll(ts.ingestURL, "", ts.credPath)
	ts.mu.Lock()
	if err != nil {
		return fail("HTTP 401 — automatic re-enrol failed: " + err.Error())
	}
	// Re-enrol rewrote the credential file; reload it.
	data, rerr := loadCredentialsFrom(ts.credPath)
	if rerr != nil {
		return fail("HTTP 401 — re-enrolled but could not reload credentials: " + rerr.Error())
	}
	ts.token = data.Token
	ts.nodeID = data.NodeID
	ts.authFails = 0
	ts.backoff = time.Time{}
	log.Printf("ingest auth: automatic re-enrol succeeded — retrying batch with the fresh token")
	return true
}

// loadCredentialsFrom reads node_id + token from a specific path.
func loadCredentialsFrom(path string) (nodeCredentials, error) {
	var creds nodeCredentials
	data, err := os.ReadFile(path)
	if err != nil {
		return creds, err
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return creds, err
	}
	return creds, nil
}

// authSender wraps a sender with 401/403 recovery (NR5). The batch is
// retried once after a successful automatic re-enrol; otherwise the error
// propagates and the watermark is left alone (nothing lost).
func authSender(ts *tokenSource, newSender func(token string) sender) sender {
	return func(path string, rows any) error {
		send := newSender(ts.get())
		err := send(path, rows)
		var se *statusError
		if !errors.As(err, &se) || (se.status != 401 && se.status != 403) {
			return err
		}
		if ts.handleAuthFailure(se.status, se.body) {
			return newSender(ts.get())(path, rows)
		}
		return err
	}
}
