package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// NR6: the metrics surface must render valid Prometheus text and a
// readable status page.

func TestPrometheusFormat(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("test_requests_total", "test counter")
	c.Add(5)
	g := r.Gauge("test_queue_depth", "test gauge")
	g.Store(42)
	r.GaugeFunc("test_computed", "computed gauge", func() int64 { return 7 })

	rec := httptest.NewRecorder()
	r.ServeMetrics(rec, nil)
	body := rec.Body.String()

	for _, want := range []string{
		`# HELP test_requests_total test counter`,
		`# TYPE test_requests_total counter`,
		`test_requests_total 5`,
		`# TYPE test_queue_depth gauge`,
		`test_queue_depth 42`,
		`test_computed 7`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q\nfull output:\n%s", want, body)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

func TestStatusPage(t *testing.T) {
	r := NewRegistry()
	r.Counter("test_requests_total", "test counter").Add(3)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	r.ServeStatus(rec, req)
	body := rec.Body.String()
	for _, want := range []string{"operator dashboard", "Contribution", "/metrics"} {
		if !strings.Contains(body, want) {
			t.Errorf("status page missing %q", want)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestCounterIdempotent(t *testing.T) {
	r := NewRegistry()
	a := r.Counter("x_total", "first")
	b := r.Counter("x_total", "second")
	a.Add(1)
	if b.Load() != 1 {
		t.Error("Counter() must return the same counter for the same name")
	}
}

// OP1: operator dashboard must render sections with health indicators.
func TestDashboardSections(t *testing.T) {
	r := NewRegistry()
	r.Counter("crawler_pages_ok_total", "pages").Add(1234)
	r.Counter("node_rows_forwarded_total", "rows").Add(567)
	r.Gauge("node_auth_state", "auth").Store(0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	r.ServeStatus(rec, req)
	body := rec.Body.String()

	for _, want := range []string{
		"operator dashboard",
		"Contribution",
		"Efficiency",
		"Politeness",
		"Problems",
		"1,234", // formatted with comma
		"Healthy",
		`http-equiv="refresh"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
}

// OP1: auth state drives health color (revoked = red).
func TestDashboardHealthRed(t *testing.T) {
	r := NewRegistry()
	r.Gauge("node_auth_state", "auth").Store(2)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	r.ServeStatus(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "Auth revoked") {
		t.Error("auth_state=2 should show 'Auth revoked'")
	}
	if !strings.Contains(body, "#e74c3c") {
		t.Error("revoked auth should use red color")
	}
}

// OP3: CSV export must be valid CSV with header.
func TestCSVExport(t *testing.T) {
	r := NewRegistry()
	r.Counter("crawler_pages_ok_total", "pages, with comma").Add(100)
	r.Gauge("node_auth_state", "auth state").Store(0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/?format=csv", nil)
	r.ServeStatus(rec, req)
	body := rec.Body.String()

	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/csv") {
		t.Errorf("Content-Type = %q, want text/csv", ct)
	}
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if lines[0] != "metric,type,value,description" {
		t.Errorf("CSV header = %q", lines[0])
	}
	// Description with comma must be quoted.
	found := false
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, "crawler_pages_ok_total,counter,100,") {
			found = true
			if !strings.Contains(l, `"pages, with comma"`) {
				t.Errorf("CSV description not quoted: %q", l)
			}
		}
	}
	if !found {
		t.Error("crawler_pages_ok_total not in CSV")
	}
}

func TestFormatNum(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0", 999: "999", 1000: "1,000",
		1234567: "1,234,567", 1000000: "1,000,000",
	} {
		if got := formatNum(in); got != want {
			t.Errorf("formatNum(%d) = %q, want %q", in, got, want)
		}
	}
}
