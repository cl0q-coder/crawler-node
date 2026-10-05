package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if err := writeCredentials(path, "nd-test1", "secret-token"); err != nil {
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
	if creds.NodeID != "nd-test1" || creds.Token != "secret-token" {
		t.Errorf("round-trip mismatch: %+v", creds)
	}
}

func TestLoadCredentialsPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	if err := writeCredentials(path, "nd-file", "file-token"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_CREDENTIALS_FILE", path)

	// Env wins over file.
	t.Setenv("NODE_TOKEN", "env-token")
	t.Setenv("CL0Q_NODE_ID", "nd-env")
	id, tok, err := loadCredentials()
	if err != nil || id != "nd-env" || tok != "env-token" {
		t.Errorf("env precedence: got (%q,%q,%v)", id, tok, err)
	}

	// File used when env absent.
	os.Unsetenv("NODE_TOKEN")
	os.Unsetenv("CL0Q_NODE_ID")
	id, tok, err = loadCredentials()
	if err != nil || id != "nd-file" || tok != "file-token" {
		t.Errorf("file fallback: got (%q,%q,%v)", id, tok, err)
	}

	// Missing file is not an error (means "not enrolled yet").
	t.Setenv("NODE_CREDENTIALS_FILE", filepath.Join(dir, "nope.json"))
	id, tok, err = loadCredentials()
	if err != nil || id != "" || tok != "" {
		t.Errorf("missing file: got (%q,%q,%v)", id, tok, err)
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
			NodeID: "nd-mock1", Token: "mock-secret", Quarantined: true,
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
	id, tok, _ := loadCredentials()
	if id != "nd-mock1" || tok != "mock-secret" {
		t.Errorf("stored credentials wrong: %q %q", id, tok)
	}
	if gotNodeID != "" {
		t.Errorf("fresh enrol should not send node_id, sent %q", gotNodeID)
	}
}

func TestRunEnrollRotationSendsNodeID(t *testing.T) {
	var gotNodeID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var er enrollRequest
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &er)
		gotNodeID = er.NodeID
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(enrollResponse{NodeID: "nd-rot1", Token: "new-secret"})
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	// Pre-existing credentials: this is a rotation.
	if err := writeCredentials(path, "nd-old1", "old-secret"); err != nil {
		t.Fatal(err)
	}
	if err := doEnroll(srv.URL, "", path); err != nil {
		t.Fatal(err)
	}
	if gotNodeID != "nd-old1" {
		t.Errorf("rotation should send existing node_id, sent %q", gotNodeID)
	}
	id, tok, _ := loadCredentials()
	// loadCredentials needs the env path override:
	t.Setenv("NODE_CREDENTIALS_FILE", path)
	id, tok, _ = loadCredentials()
	if id != "nd-rot1" || tok != "new-secret" {
		t.Errorf("rotated credentials wrong: %q %q", id, tok)
	}
}
