package main

import (
	"testing"
)

// OP2: Prometheus parser must handle the text format correctly.

func TestParsePrometheus(t *testing.T) {
	text := `# HELP crawler_pages_ok_total pages
# TYPE crawler_pages_ok_total counter
crawler_pages_ok_total 1234
# HELP node_auth_state auth
# TYPE node_auth_state gauge
node_auth_state 0
# comment line

crawler_db_domains 5678
`
	m := parsePrometheus(text)
	if m["crawler_pages_ok_total"] != 1234 {
		t.Errorf("pages_ok = %d, want 1234", m["crawler_pages_ok_total"])
	}
	if m["node_auth_state"] != 0 {
		t.Errorf("auth_state = %d, want 0", m["node_auth_state"])
	}
	if m["crawler_db_domains"] != 5678 {
		t.Errorf("db_domains = %d, want 5678", m["crawler_db_domains"])
	}
	if len(m) != 3 {
		t.Errorf("parsed %d metrics, want 3", len(m))
	}
}

func TestParsePrometheusInvalid(t *testing.T) {
	m := parsePrometheus("not a metric line\n\n# just a comment\n")
	if len(m) != 0 {
		t.Errorf("invalid input should parse to empty, got %v", m)
	}
}

func TestFormatNumCLI(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567",
	} {
		if got := formatNumCLI(in); got != want {
			t.Errorf("formatNumCLI(%d) = %q, want %q", in, got, want)
		}
	}
}
