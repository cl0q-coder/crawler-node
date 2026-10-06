package warc

import (
	"bytes"
	"strings"
	"testing"
)

// IG4: WARC writer must produce valid WARC/1.0 records.

func TestWriteInfo(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf)
	if err := w.WriteInfo("test-operator", "cl0q-test/1.0"); err != nil {
		t.Fatalf("WriteInfo error: %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "WARC/1.0\r\n") {
		t.Error("WARC file must start with WARC/1.0")
	}
	if !strings.Contains(out, "WARC-Type: warcinfo") {
		t.Error("missing WARC-Type: warcinfo")
	}
	if !strings.Contains(out, "WARC-Record-ID: <urn:uuid:") {
		t.Error("missing WARC-Record-ID")
	}
	if !strings.Contains(out, "software: cl0q-test/1.0") {
		t.Error("missing software field")
	}
}

func TestWriteResponse(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf)
	content := []byte("HTTP/1.1 200 OK\r\nContent-Type: text/html\r\n\r\n<html></html>")
	if err := w.WriteResponse("https://example.com/", content, ""); err != nil {
		t.Fatalf("WriteResponse error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "WARC-Type: response") {
		t.Error("missing WARC-Type: response")
	}
	if !strings.Contains(out, "WARC-Target-URI: https://example.com/") {
		t.Error("missing WARC-Target-URI")
	}
	if !strings.Contains(out, string(content)) {
		t.Error("response content not in WARC record")
	}
	// Records end with \r\n\r\n
	if !strings.HasSuffix(out, "\r\n\r\n") {
		t.Error("WARC record must end with CRLF CRLF")
	}
}

func TestDigest(t *testing.T) {
	d := Digest([]byte("hello"))
	if !strings.HasPrefix(d, "sha1:") {
		t.Errorf("Digest should start with sha1:, got %q", d)
	}
	// SHA-1 of "hello" is known.
	if d != "sha1:aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d" {
		t.Errorf("wrong digest: %q", d)
	}
}

func TestNewUUID(t *testing.T) {
	a := newUUID()
	b := newUUID()
	if a == b {
		t.Error("UUIDs must be unique")
	}
	// Format: 8-4-4-4-12 hex chars
	parts := strings.Split(a, "-")
	if len(parts) != 5 {
		t.Errorf("UUID should have 5 parts, got %d", len(parts))
	}
	// Version 4 (3rd part starts with 4)
	if !strings.HasPrefix(parts[2], "4") {
		t.Errorf("UUID should be version 4, got %q", a)
	}
}
