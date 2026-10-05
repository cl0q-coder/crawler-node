package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// NR: enrol client tests (PROTOCOL.md §1). The server side doesn't exist
// yet, so a mock stands in for cl0q-ingest.

func TestWriteCredentialsMode0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	// Pre-create with wide perms: writeCredentials must force 0600 anyway.
	if err := os.WriteFile(path, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(path, nodeCredentials{NodeID: "nd-test1", Token: "secret-token", RefreshSecret: "rs-test1"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("credentials mode = %o, want 0600", fi.Mode().Perm())
	}
	var creds nodeCredentials
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &creds); err != nil {
		t.Fatal(err)
	}
	if creds.NodeID != "nd-test1" || creds.Token != "secret-token" || creds.RefreshSecret != "rs-test1" {
		t.Errorf("round-trip mismatch: %+v", creds)
	}
}

func TestLoadCredentialsPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	if err := writeCredentials(path, nodeCredentials{NodeID: "nd-file", Token: "file-token", RefreshSecret: "rs-file"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_CREDENTIALS_FILE", path)

	// Env wins over file (env carries no refresh_secret — it cannot rotate).
	t.Setenv("NODE_TOKEN", "env-token")
	t.Setenv("CL0Q_NODE_ID", "nd-env")
	creds, err := loadCredentials()
	if err != nil || creds.NodeID != "nd-env" || creds.Token != "env-token" || creds.RefreshSecret != "" {
		t.Errorf("env precedence: got (%+v,%v)", creds, err)
	}

	// File used when env absent.
	os.Unsetenv("NODE_TOKEN")
	os.Unsetenv("CL0Q_NODE_ID")
	creds, err = loadCredentials()
	if err != nil || creds.NodeID != "nd-file" || creds.Token != "file-token" || creds.RefreshSecret != "rs-file" {
		t.Errorf("file fallback: got (%+v,%v)", creds, err)
	}

	// Missing file is not an error (means "not enrolled yet").
	t.Setenv("NODE_CREDENTIALS_FILE", filepath.Join(dir, "nope.json"))
	creds, err = loadCredentials()
	if err != nil || creds.NodeID != "" || creds.Token != "" {
		t.Errorf("missing file: got (%+v,%v)", creds, err)
	}
}

func TestRunEnrollRoundTrip(t *testing.T) {
	var gotBody []byte
	var gotNodeID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/nodes/enroll" {
			t.Errorf("wrong path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("wrong method: %s", r.Method)
		}
		gotBody, _ = io.ReadAll(r.Body)
		var er enrollRequest
		json.Unmarshal(gotBody, &er)
		gotNodeID = er.NodeID
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(enrollResponse{
			NodeID: "nd-mock1", Token: "mock-secret", RefreshSecret: "rs-mock1", Quarantined: true,
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	t.Setenv("INGEST_URL", srv.URL)
	t.Setenv("NODE_CREDENTIALS_FILE", path)

	// runEnrollCommand os.Exits on failure; success returns normally.
	// Call the pieces directly to stay in-process.
	if err := doEnroll(srv.URL, "op@example.com", path); err != nil {
		t.Fatal(err)
	}
	// Request shape per PROTOCOL.md.
	var sent enrollRequest
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Version == "" || sent.OperatorContact != "op@example.com" {
		t.Errorf("bad enrol request: %+v", sent)
	}
	// Credentials stored, 0600.
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0600 {
		t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
	}
	creds, _ := loadCredentials()
	if creds.NodeID != "nd-mock1" || creds.Token != "mock-secret" || creds.RefreshSecret != "rs-mock1" {
		t.Errorf("stored credentials wrong: %+v", creds)
	}
	if gotNodeID != "" {
		t.Errorf("fresh enrol should not send node_id, sent %q", gotNodeID)
	}
}

func TestRunEnrollRejectsMissingRefreshSecret(t *testing.T) {
	// A server that omits refresh_secret violates PROTOCOL.md §1 —
	// enrol must fail loudly rather than store a half credential.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(enrollResponse{NodeID: "nd-mock2", Token: "mock-secret"})
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	if err := doEnroll(srv.URL, "", path); err == nil {
		t.Fatal("enrol without refresh_secret should fail")
	} else if !strings.Contains(err.Error(), "refresh_secret") {
		t.Errorf("error should mention refresh_secret, got %q", err.Error())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("failed enrol must not leave a credential file behind")
	}
}

func TestRunEnrollRotationSendsNodeIDAndRefreshSecret(t *testing.T) {
	var got enrollRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(enrollResponse{NodeID: "nd-rot1", Token: "new-secret", RefreshSecret: "rs-rot1"})
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	// Pre-existing credentials: this is a rotation.
	if err := writeCredentials(path, nodeCredentials{NodeID: "nd-old1", Token: "old-secret", RefreshSecret: "rs-old1"}); err != nil {
		t.Fatal(err)
	}
	if err := doEnroll(srv.URL, "", path); err != nil {
		t.Fatal(err)
	}
	if got.NodeID != "nd-old1" || got.RefreshSecret != "rs-old1" {
		t.Errorf("rotation must send node_id + refresh_secret, sent %+v", got)
	}
	t.Setenv("NODE_CREDENTIALS_FILE", path)
	creds, _ := loadCredentials()
	if creds.NodeID != "nd-rot1" || creds.Token != "new-secret" || creds.RefreshSecret != "rs-rot1" {
		t.Errorf("rotated credentials wrong: %+v", creds)
	}
}

func TestRunEnrollLegacyCredsFallBackToFreshEnrol(t *testing.T) {
	// A credential file from before refresh_secret existed cannot
	// authenticate rotation — the client must NOT send the bare node_id
	// (PROTOCOL.md §1). It fresh-enrols instead.
	var got enrollRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(enrollResponse{NodeID: "nd-fresh9", Token: "fresh-secret", RefreshSecret: "rs-fresh9"})
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	legacy, _ := json.Marshal(nodeCredentials{NodeID: "nd-legacy", Token: "legacy-secret"})
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if err := doEnroll(srv.URL, "", path); err != nil {
		t.Fatal(err)
	}
	if got.NodeID != "" || got.RefreshSecret != "" {
		t.Errorf("legacy creds must trigger fresh enrol (no node_id sent), sent %+v", got)
	}
}

// D1 (P1 #4): the enrol client must never follow redirects — the request
// carries the refresh_secret on re-enrol, and following a 3xx would leak it
// to the redirect target.
func TestDoEnrollRefusesRedirect(t *testing.T) {
	var redirectHit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectHit = true // the secret must never reach here
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Evil/misconfigured server: redirect the enrol POST elsewhere.
		http.Redirect(w, r, target.URL+"/api/nodes/enroll", http.StatusFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	// Pre-seed credentials so this is a re-enrol carrying the refresh_secret.
	creds, _ := json.Marshal(nodeCredentials{NodeID: "nd-test1", Token: "tok", RefreshSecret: "rs-super-secret"})
	if err := os.WriteFile(path, creds, 0600); err != nil {
		t.Fatal(err)
	}

	err := doEnroll(srv.URL, "op@example.com", path)
	if err == nil {
		t.Fatal("doEnroll must fail on 3xx redirect, got nil error")
	}
	if redirectHit {
		t.Fatal("redirect target was hit — refresh_secret may have leaked")
	}
	// No credentials file should have been written from a redirect response.
	if _, statErr := os.Stat(path); statErr == nil {
		data, _ := os.ReadFile(path)
		var c nodeCredentials
		json.Unmarshal(data, &c)
		if c.NodeID != "nd-test1" {
			t.Errorf("credentials file was overwritten by redirect flow: %+v", c)
		}
	}
}
