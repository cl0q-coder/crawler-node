// Enrol client — implements PROTOCOL.md §1 (open enrol, model A).
//
//	Usage: cl0q-node enroll [--contact you@example.com] [--out path]
//
// POSTs to {INGEST_URL}/api/nodes/enroll, stores the returned node_id +
// token as JSON at the credentials path with mode 0600, and prints the
// node id (never the token). If a credentials file already exists, its
// node_id is sent along, making re-enrol an idempotent rotation.
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
)

type enrollRequest struct {
	Version         string `json:"version"`
	OperatorContact string `json:"operator_contact,omitempty"`
	NodeID          string `json:"node_id,omitempty"` // set on re-enrol (rotation)
}

type enrollResponse struct {
	NodeID      string `json:"node_id"`
	Token       string `json:"token"`
	IngestURL   string `json:"ingest_url"`
	Quarantined bool   `json:"quarantined"`
}

// nodeCredentials is what lives in the credentials file (0600).
type nodeCredentials struct {
	NodeID string `json:"node_id"`
	Token  string `json:"token"`
}

// credentialsPath is where enrol stores node_id + token.
// NODE_CREDENTIALS_FILE wins; otherwise ./cl0q-node.json in the cwd.
func credentialsPath() string {
	if p := os.Getenv("NODE_CREDENTIALS_FILE"); p != "" {
		return p
	}
	return "cl0q-node.json"
}

// loadCredentials returns (node_id, token). Precedence: NODE_TOKEN /
// CL0Q_NODE_ID env, then the credentials file. Missing file is not an
// error — it just means "not enrolled yet".
func loadCredentials() (nodeID, token string, err error) {
	if token = os.Getenv("NODE_TOKEN"); token != "" {
		return os.Getenv(botid.EnvNodeID), token, nil
	}
	data, err := os.ReadFile(credentialsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("read credentials %s: %w", credentialsPath(), err)
	}
	var creds nodeCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", "", fmt.Errorf("parse credentials %s: %w", credentialsPath(), err)
	}
	return creds.NodeID, creds.Token, nil
}

// writeCredentials stores node_id + token as JSON with mode 0600.
// The file must not exist with wider permissions afterwards.
func writeCredentials(path, nodeID, token string) error {
	data, err := json.MarshalIndent(nodeCredentials{NodeID: nodeID, Token: token}, "", "  ")
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

// doEnroll performs the enrol handshake and stores the credentials.
// The token is never printed or logged. Returns nil on success.
func doEnroll(ingestURL, contact, out string) error {
	// Re-enrol (rotation) if we already have credentials: send the node_id.
	var reNodeID string
	if data, err := os.ReadFile(out); err == nil {
		var creds nodeCredentials
		if json.Unmarshal(data, &creds) == nil {
			reNodeID = creds.NodeID
		}
	}

	reqBody, _ := json.Marshal(enrollRequest{
		Version:         botid.Product + "/" + botid.Version,
		OperatorContact: contact,
		NodeID:          reNodeID,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ingestURL+"/api/nodes/enroll", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cl0q-node-enroll/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s/api/nodes/enroll: %w", ingestURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
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
	if _, err := botid.UserAgent(er.NodeID); err != nil {
		return fmt.Errorf("server returned invalid node_id: %w", err)
	}
	if err := writeCredentials(out, er.NodeID, er.Token); err != nil {
		return err
	}
	// The token is NEVER printed.
	quarantine := "quarantined (submissions are spot-verified before reaching the live index)"
	if !er.Quarantined {
		quarantine = "trusted"
	}
	fmt.Printf("enrolled as node %s — %s\ncredentials stored at %s (mode 0600)\nnext: set CL0Q_NODE_ID=%s (or it is read from the credentials file) and run preflight\n",
		er.NodeID, quarantine, out, er.NodeID)
	return nil
}
