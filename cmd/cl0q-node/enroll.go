// Enrol client — implements PROTOCOL.md §1 (open enrol, model A).
//
//	Usage: cl0q-node enroll [--contact you@example.com] [--out path]
//
// POSTs to {INGEST_URL}/api/nodes/enroll, stores the returned node_id +
// token + refresh_secret as JSON at the credentials path with mode 0600,
// and prints the node id (never the token or refresh_secret). If a
// credentials file already exists, its node_id + refresh_secret are sent
// along, making re-enrol an authenticated idempotent rotation
// (PROTOCOL.md §1: the node_id alone must never authorize rotation).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"cl0q/botid"
	"cl0q/internal/trust"
)

type enrollRequest struct {
	Version         string `json:"version"`
	OperatorContact string `json:"operator_contact,omitempty"`
	NodeID          string `json:"node_id,omitempty"`        // set on re-enrol (rotation)
	RefreshSecret   string `json:"refresh_secret,omitempty"` // required with NodeID on re-enrol
}

type enrollResponse struct {
	NodeID        string `json:"node_id"`
	Token         string `json:"token"`
	RefreshSecret string `json:"refresh_secret"`
	IngestURL     string `json:"ingest_url"`
	Quarantined   bool   `json:"quarantined"`
	// PowChallenge, when present, is a G-12 proof-of-work Sybil gate: the
	// client must solve it and confirm before credentials are trusted.
	// Absent = server doesn't require PoW; enrollment proceeds unchanged.
	PowChallenge *trust.Challenge `json:"pow_challenge,omitempty"`
}

// nodeCredentials is what lives in the credentials file (0600). The
// refresh_secret authenticates rotation: it is stored like the token
// (never logged, never printed, never in a User-Agent).
type nodeCredentials struct {
	NodeID        string `json:"node_id"`
	Token         string `json:"token"`
	RefreshSecret string `json:"refresh_secret,omitempty"`
}

// credentialsPath is where enrol stores node_id + token.
// NODE_CREDENTIALS_FILE wins; otherwise ./cl0q-node.json in the cwd.
func credentialsPath() string {
	if p := os.Getenv("NODE_CREDENTIALS_FILE"); p != "" {
		return p
	}
	return "cl0q-node.json"
}

// loadCredentials returns the stored credentials. Precedence: NODE_TOKEN /
// CL0Q_NODE_ID env, then the credentials file. Missing file is not an
// error — it just means "not enrolled yet". The env path carries no
// refresh_secret (it was never stored there), so env credentials cannot
// rotate.
func loadCredentials() (nodeCredentials, error) {
	var creds nodeCredentials
	if token := os.Getenv("NODE_TOKEN"); token != "" {
		creds.NodeID = os.Getenv(botid.EnvNodeID)
		creds.Token = token
		return creds, nil
	}
	data, err := os.ReadFile(credentialsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return creds, nil
		}
		return creds, fmt.Errorf("read credentials %s: %w", credentialsPath(), err)
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return creds, fmt.Errorf("parse credentials %s: %w", credentialsPath(), err)
	}
	return creds, nil
}

// writeCredentials stores the credentials as JSON with mode 0600.
// The file must not exist with wider permissions afterwards.
func writeCredentials(path string, creds nodeCredentials) error {
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	// Belt and braces: an existing file keeps its old mode on O_TRUNC,
	// so force 0600 in case it was created wider by hand.
	if err := os.Chmod(path, 0600); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

// runEnrollCommand implements `cl0q-node enroll`.
func runEnrollCommand(args []string) {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	contact := fs.String("contact", "", "operator contact email (optional, recommended — how cl0q reaches you about abuse reports)")
	out := fs.String("out", credentialsPath(), "where to store node_id + token (mode 0600)")
	fs.Parse(args)

	ingestURL := os.Getenv("INGEST_URL")
	if ingestURL == "" {
		fmt.Fprintln(os.Stderr, "enroll: INGEST_URL is not set")
		os.Exit(2)
	}
	var err error
	if ingestURL, err = checkIngestURL(ingestURL, os.Getenv("CL0Q_NODE_ALLOW_HTTP") == "1"); err != nil {
		fmt.Fprintln(os.Stderr, "enroll:", err)
		os.Exit(2)
	}
	if err := doEnroll(ingestURL, *contact, *out); err != nil {
		fmt.Fprintln(os.Stderr, "enroll:", err)
		os.Exit(1)
	}
}

// enrollClient is the HTTP client for the enrol handshake. It NEVER follows
// redirects: the request body carries the refresh_secret on re-enrol, and
// following a 3xx would leak it to the redirect target. A 3xx response is
// returned as-is so the caller sees the refusal.
var enrollClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// doEnroll performs the enrol handshake and stores the credentials.
// The token and refresh_secret are never printed or logged. Returns nil
// on success.
func doEnroll(ingestURL, contact, out string) error {
	// Re-enrol (rotation) if we already have credentials: send node_id +
	// refresh_secret. A credential file from before refresh_secret existed
	// cannot authenticate rotation — fall back to a fresh enrol rather
	// than sending the node_id alone (PROTOCOL.md §1).
	var reCreds nodeCredentials
	if data, err := os.ReadFile(out); err == nil {
		var creds nodeCredentials
		if json.Unmarshal(data, &creds) == nil && creds.RefreshSecret != "" {
			reCreds = creds
		}
	}

	reqBody, _ := json.Marshal(enrollRequest{
		Version:         botid.Product + "/" + botid.Version,
		OperatorContact: contact,
		NodeID:          reCreds.NodeID,
		RefreshSecret:   reCreds.RefreshSecret,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ingestURL+"/api/nodes/enroll", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cl0q-node-enroll/1.0")

	resp, err := enrollClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s/api/nodes/enroll: %w", ingestURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if resp.StatusCode >= 300 {
		// 3xx: the server tried to redirect us; enrollClient refuses to follow
		// (the request carried the refresh_secret). Treat as a hard failure —
		// never silently send credentials elsewhere.
		return fmt.Errorf("server returned HTTP %d (redirects are not followed): %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	var er enrollResponse
	if err := json.Unmarshal(body, &er); err != nil {
		return fmt.Errorf("bad response: %w", err)
	}
	if er.NodeID == "" || er.Token == "" {
		return fmt.Errorf("server response missing node_id or token")
	}
	if er.RefreshSecret == "" {
		return fmt.Errorf("server response missing refresh_secret (PROTOCOL.md §1)")
	}
	if _, err := botid.UserAgent(er.NodeID); err != nil {
		return fmt.Errorf("server returned invalid node_id: %w", err)
	}
	// G-12 Sybil gate: if the server issued a proof-of-work challenge, solve
	// it and confirm before the credentials are usable. Backward compatible:
	// no challenge in the response = no PoW required.
	if er.PowChallenge != nil {
		if err := confirmPoW(ingestURL, er.NodeID, *er.PowChallenge); err != nil {
			return fmt.Errorf("proof-of-work confirm: %w", err)
		}
	}
	if err := writeCredentials(out, nodeCredentials{
		NodeID:        er.NodeID,
		Token:         er.Token,
		RefreshSecret: er.RefreshSecret,
	}); err != nil {
		return err
	}
	// The token and refresh_secret are NEVER printed.
	quarantine := "quarantined (submissions are spot-verified before reaching the live index)"
	if !er.Quarantined {
		quarantine = "trusted"
	}
	fmt.Printf("enrolled as node %s — %s\ncredentials stored at %s (mode 0600)\nnext: set CL0Q_NODE_ID=%s (or it is read from the credentials file) and run preflight\n",
		er.NodeID, quarantine, out, er.NodeID)
	return nil
}

// powConfirmRequest is sent to POST /api/nodes/enroll/confirm after solving
// the server's challenge. The server re-verifies with trust.VerifySolution
// (one hash) and only then activates the node.
type powConfirmRequest struct {
	NodeID   string `json:"node_id"`
	PowNonce uint64 `json:"pow_nonce"`
}

func confirmPoW(ingestURL, nodeID string, ch trust.Challenge) error {
	fmt.Printf("server requires proof-of-work (difficulty %d bits) — solving...\n", ch.Difficulty)
	nonce, err := trust.SolveChallenge(ch, nodeID)
	if err != nil {
		return err
	}
	reqBody, _ := json.Marshal(powConfirmRequest{NodeID: nodeID, PowNonce: nonce})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ingestURL+"/api/nodes/enroll/confirm", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cl0q-node-enroll/1.0")
	resp, err := enrollClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s/api/nodes/enroll/confirm: %w", ingestURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	fmt.Println("proof-of-work accepted — node activated")
	return nil
}
