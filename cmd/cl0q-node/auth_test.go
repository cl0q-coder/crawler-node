package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// NR5: break-it tests for token auth hardening. The ingest is a public
// boundary; every failure mode must degrade gracefully, never spin, and
// never leak the token into logs.

func TestIsRevoked(t *testing.T) {
	if !isRevoked(`{"revoked": true}`) {
		t.Error(`{"revoked": true} should be revoked`)
	}
	for _, body := range []string{`{"revoked": false}`, `{}`, `not json`, ``} {
		if isRevoked(body) {
			t.Errorf("%q should NOT be revoked", body)
		}
	}
}

func TestAuthBackoffFor(t *testing.T) {
	if got := authBackoffFor(0); got != time.Minute {
		t.Errorf("backoff(0) = %s, want 1m", got)
	}
	if got := authBackoffFor(2); got != 4*time.Minute {
		t.Errorf("backoff(2) = %s, want 4m", got)
	}
	if got := authBackoffFor(100); got != time.Hour {
		t.Errorf("backoff(100) = %s, want capped at 1h", got)
	}
}

// mockEnrollServer returns a server that acts as /api/nodes/enroll.
// On rotation requests (node_id present) it requires the refresh_secret.
func mockEnrollServer(t *testing.T, nodeID, token, refreshSecret string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			var req enrollRequest
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &req)
			if req.NodeID != "" && req.RefreshSecret == "" {
				t.Errorf("rotation request for node %q missing refresh_secret", req.NodeID)
			}
			json.NewEncoder(w).Encode(enrollResponse{NodeID: nodeID, Token: token, RefreshSecret: refreshSecret})
		} else {
			w.Write([]byte(`{"error": "nope"}`))
		}
	}))
}

func TestHandleAuthFailure401AutoReenroll(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "creds.json")
	if err := writeCredentials(credPath, nodeCredentials{NodeID: "nd-old9", Token: "expired-token", RefreshSecret: "rs-old9"}); err != nil {
		t.Fatal(err)
	}
	srv := mockEnrollServer(t, "nd-old9", "fresh-token", "fresh-refresh", http.StatusOK)
	defer srv.Close()

	ts := &tokenSource{token: "expired-token", refreshSecret: "rs-old9", nodeID: "nd-old9", credPath: credPath, ingestURL: srv.URL}
	if !ts.handleAuthFailure(401, "") {
		t.Fatal("successful re-enrol should return retry=true")
	}
	if got := ts.get(); got != "fresh-token" {
		t.Errorf("token not rotated: got %q", got)
	}
	if ts.refreshSecret != "fresh-refresh" {
		t.Errorf("refresh_secret not rotated: got %q", ts.refreshSecret)
	}
	if ts.authFails != 0 {
		t.Errorf("authFails should reset after successful re-enrol, got %d", ts.authFails)
	}
	// Credential file updated too.
	creds, _ := loadCredentialsFrom(credPath)
	if creds.NodeID != "nd-old9" || creds.Token != "fresh-token" || creds.RefreshSecret != "fresh-refresh" {
		t.Errorf("credential file not updated: %+v", creds)
	}
}

func TestHandleAuthFailure401ReenrollFailsBacksOff(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "creds.json")
	if err := writeCredentials(credPath, nodeCredentials{NodeID: "nd-old9", Token: "expired-token", RefreshSecret: "rs-old9"}); err != nil {
		t.Fatal(err)
	}
	srv := mockEnrollServer(t, "", "", "", http.StatusInternalServerError)
	defer srv.Close()

	ts := &tokenSource{token: "expired-token", nodeID: "nd-old9", credPath: credPath, ingestURL: srv.URL}
	if ts.handleAuthFailure(401, "") {
		t.Error("failed re-enrol should return retry=false")
	}
	if ts.authFails != 1 {
		t.Errorf("authFails = %d, want 1", ts.authFails)
	}
	if wait := ts.authBackoff(); wait <= 0 || wait > 2*time.Minute {
		t.Errorf("backoff = %s, want ~1m", wait)
	}
	// Token unchanged (still the old one; next cycle backs off instead of spinning).
	if got := ts.get(); got != "expired-token" {
		t.Errorf("token should be unchanged after failed re-enrol, got %q", got)
	}
}

func TestHandleAuthFailure401EnvTokenNoRotation(t *testing.T) {
	ts := &tokenSource{token: "env-token", nodeID: "nd-env9", credPath: "", ingestURL: "https://cl0q.com"}
	if ts.handleAuthFailure(401, "") {
		t.Error("env token cannot auto-rotate: should return retry=false")
	}
	if ts.authFails != 1 {
		t.Errorf("authFails = %d, want 1", ts.authFails)
	}
}

func TestHandleAuthFailure403RevokedWipesToken(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "creds.json")
	if err := writeCredentials(credPath, nodeCredentials{NodeID: "nd-old9", Token: "dead-token", RefreshSecret: "rs-old9"}); err != nil {
		t.Fatal(err)
	}
	ts := &tokenSource{token: "dead-token", refreshSecret: "rs-old9", nodeID: "nd-old9", credPath: credPath, ingestURL: "https://cl0q.com"}
	if ts.handleAuthFailure(403, `{"revoked": true}`) {
		t.Error("revoked token should return retry=false")
	}
	if got := ts.get(); got != "" {
		t.Errorf("revoked token should be wiped in memory, got %q", got)
	}
	creds, _ := loadCredentialsFrom(credPath)
	if creds.Token != "" {
		t.Errorf("revoked token should be wiped from file, got %q", creds.Token)
	}
	// node_id + refresh_secret kept: `cl0q-node enroll` attempts an
	// authenticated rotation and the server decides whether the node
	// may come back.
	if creds.NodeID != "nd-old9" {
		t.Errorf("node_id should survive revocation wipe, got %q", creds.NodeID)
	}
	if creds.RefreshSecret != "rs-old9" {
		t.Errorf("refresh_secret should survive revocation wipe, got %q", creds.RefreshSecret)
	}
	if wait := ts.authBackoff(); wait < 59*time.Minute {
		t.Errorf("revoked backoff = %s, want ~1h", wait)
	}
}

func TestHandleAuthFailure403NoFlagIsIPMismatch(t *testing.T) {
	ts := &tokenSource{token: "tok", nodeID: "nd-x1", credPath: "", ingestURL: "https://cl0q.com"}
	if ts.handleAuthFailure(403, `{"error": "forbidden"}`) {
		t.Error("should return retry=false")
	}
	if ts.authFails != 1 {
		t.Errorf("authFails = %d, want 1", ts.authFails)
	}
}

// TestAuthSenderRetriesAfterReenroll: the sender wrapper retries the batch
// once with the fresh token after a 401.
func TestAuthSenderRetriesAfterReenroll(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "creds.json")
	if err := writeCredentials(credPath, nodeCredentials{NodeID: "nd-old9", Token: "expired-token", RefreshSecret: "rs-old9"}); err != nil {
		t.Fatal(err)
	}
	enrollSrv := mockEnrollServer(t, "nd-old9", "fresh-token", "fresh-refresh", http.StatusOK)
	defer enrollSrv.Close()

	var calls int
	var gotTokens []string
	fakeInner := func(tok string) sender {
		return func(path string, rows any) error {
			calls++
			gotTokens = append(gotTokens, tok)
			if calls == 1 {
				return &statusError{status: 401}
			}
			return nil
		}
	}
	ts := &tokenSource{token: "expired-token", nodeID: "nd-old9", credPath: credPath, ingestURL: enrollSrv.URL}
	send := authSender(ts, fakeInner)
	if err := send("/api/ingest/website", []string{"row"}); err != nil {
		t.Fatalf("send after re-enrol should succeed: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (initial + retry)", calls)
	}
	if len(gotTokens) != 2 || gotTokens[0] != "expired-token" || gotTokens[1] != "fresh-token" {
		t.Errorf("tokens used: %v, want [expired-token fresh-token]", gotTokens)
	}
}

// TestAuthSenderNoTokenInErrors: the token must never appear in returned
// errors or (by construction) logs.
func TestAuthSenderNoTokenInErrors(t *testing.T) {
	ts := &tokenSource{token: "super-secret-token", credPath: "", ingestURL: "https://cl0q.com"}
	send := authSender(ts, func(tok string) sender {
		return func(path string, rows any) error {
			return &statusError{status: 401, body: "unauthorized"}
		}
	})
	err := send("/x", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var se *statusError
	if !errors.As(err, &se) {
		t.Fatalf("expected statusError, got %T", err)
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Error("token leaked into error")
	}
}
