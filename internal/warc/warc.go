// Package warc writes WARC 1.0 files (ISO 28500) for archival export (IG4).
// WARC is the lingua franca of web archives (Heritrix, Common Crawl,
// Internet Archive). Optional WARC export buys credibility with the
// archival/research community.
//
// This is a minimal writer: warcinfo + response records. It does not
// implement the full spec (revisit records, etc.).
package warc

import (
	"crypto/rand"
	"crypto/sha1"
	"fmt"
	"io"
	"time"
)

const version = "WARC/1.0"

// Writer writes WARC records to an underlying writer.
type Writer struct {
	w io.Writer
}

// New creates a Writer. Call WriteInfo first to write the warcinfo record.
func New(w io.Writer) *Writer {
	return &Writer{w: w}
}

// WriteInfo writes the warcinfo record describing this WARC file.
func (w *Writer) WriteInfo(operator, software string) error {
	fields := map[string]string{
		"software": software,
		"operator": operator,
		"format":   "WARC File Format 1.0",
	}
	return w.writeRecord("warcinfo", "", "application/warc-fields", fields, nil)
}

// WriteResponse writes a response record for a fetched URL.
// content is the raw HTTP response (headers + body).
func (w *Writer) WriteResponse(url string, content []byte, contentType string) error {
	if contentType == "" {
		contentType = "application/http; msgtype=response"
	}
	fields := map[string]string{
		"WARC-Target-URI": url,
	}
	return w.writeRecord("response", url, contentType, fields, content)
}

// writeRecord writes a single WARC record.
func (w *Writer) writeRecord(recordType, targetURI, contentType string, extraFields map[string]string, content []byte) error {
	id := newUUID()
	date := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	// Build header.
	header := fmt.Sprintf("%s\r\n", version)
	header += fmt.Sprintf("WARC-Type: %s\r\n", recordType)
	header += fmt.Sprintf("WARC-Date: %s\r\n", date)
	header += fmt.Sprintf("WARC-Record-ID: <urn:uuid:%s>\r\n", id)
	if targetURI != "" {
		header += fmt.Sprintf("WARC-Target-URI: %s\r\n", targetURI)
	}
	header += fmt.Sprintf("Content-Type: %s\r\n", contentType)
	header += fmt.Sprintf("Content-Length: %d\r\n", len(content))
	for k, v := range extraFields {
		// Skip WARC-Target-URI if already written.
		if k == "WARC-Target-URI" && targetURI != "" {
			continue
		}
		header += fmt.Sprintf("%s: %s\r\n", k, v)
	}
	header += "\r\n"

	// Write header, content, and record separator.
	if _, err := io.WriteString(w.w, header); err != nil {
		return err
	}
	if len(content) > 0 {
		if _, err := w.w.Write(content); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w.w, "\r\n\r\n")
	return err
}

// Digest returns the SHA-1 digest of content (for WARC-Payload-Digest).
func Digest(content []byte) string {
	h := sha1.Sum(content)
	return fmt.Sprintf("sha1:%x", h)
}

// newUUID generates a random UUID v4.
func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
