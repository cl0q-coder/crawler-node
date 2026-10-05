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
	r.ServeStatus(rec, nil)
	body := rec.Body.String()
	for _, want := range []string{"<table", "test_requests_total", ">3<", "/metrics"} {
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
