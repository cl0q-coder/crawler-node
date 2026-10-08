// Package metrics is the node's observability surface (NR6): a
// Prometheus-text-format /metrics endpoint plus a human-readable /status
// page. Operators see node health without reading logs.
//
// Enable with METRICS_ADDR (e.g. "127.0.0.1:9100"); unset means no server.
// Each binary registers its own counters/gauges and serves them.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds counters and gauges for one process.
type Registry struct {
	mu       sync.Mutex
	counters map[string]*atomic.Int64
	gauges   map[string]*atomic.Int64
	help     map[string]string
	// gaugeFuncs are computed on scrape (e.g. DB sizes, queue depths).
	gaugeFuncs map[string]func() int64
}

func NewRegistry() *Registry {
	return &Registry{
		counters:   map[string]*atomic.Int64{},
		gauges:     map[string]*atomic.Int64{},
		help:       map[string]string{},
		gaugeFuncs: map[string]func() int64{},
	}
}

// Counter returns the named counter, creating it. Name should be
// snake_case; help is a one-line description.
func (r *Registry) Counter(name, help string) *atomic.Int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name]; ok {
		return c
	}
	c := &atomic.Int64{}
	r.counters[name] = c
	r.help[name] = help
	return c
}

// Gauge returns the named gauge, creating it.
func (r *Registry) Gauge(name, help string) *atomic.Int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.gauges[name]; ok {
		return g
	}
	g := &atomic.Int64{}
	r.gauges[name] = g
	r.help[name] = help
	return g
}

// GaugeFunc registers a gauge computed at scrape time.
func (r *Registry) GaugeFunc(name, help string, fn func() int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gaugeFuncs[name] = fn
	r.help[name] = help
}

// snapshot returns all current values, sorted by name.
func (r *Registry) snapshot() (counters, gauges map[string]int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	counters = make(map[string]int64, len(r.counters))
	for name, c := range r.counters {
		counters[name] = c.Load()
	}
	gauges = make(map[string]int64, len(r.gauges)+len(r.gaugeFuncs))
	for name, g := range r.gauges {
		gauges[name] = g.Load()
	}
	for name, fn := range r.gaugeFuncs {
		gauges[name] = fn()
	}
	return counters, gauges
}

// ServeMetrics writes the Prometheus text format.
func (r *Registry) ServeMetrics(w http.ResponseWriter, _ *http.Request) {
	counters, gauges := r.snapshot()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	names := []string{}
	for n := range counters {
		names = append(names, n)
	}
	for n := range gauges {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		if h := r.help[n]; h != "" {
			fmt.Fprintf(&b, "# HELP %s %s\n", n, h)
		}
		if v, ok := counters[n]; ok {
			fmt.Fprintf(&b, "# TYPE %s counter\n%s %d\n", n, n, v)
		} else {
			fmt.Fprintf(&b, "# TYPE %s gauge\n%s %d\n", n, n, gauges[n])
		}
	}
	w.Write([]byte(b.String()))
}

// ServeStatus writes the operator dashboard: grouped metrics with health
// indicators, auto-refresh. This is the node's window for operators —
// health, contribution, efficiency, politeness, problems — without exposing
// the search index or row contents.
func (r *Registry) ServeStatus(w http.ResponseWriter, req *http.Request) {
	// CSV export: /?format=csv
	if req.URL.Query().Get("format") == "csv" {
		r.ServeCSV(w)
		return
	}
	counters, gauges := r.snapshot()
	get := func(name string) int64 {
		if v, ok := counters[name]; ok {
			return v
		}
		return gauges[name]
	}

	// Health: auth state drives the header color.
	authState := get("node_auth_state")
	healthColor, healthText := "#2ecc71", "Healthy"
	if authState == 1 {
		healthColor, healthText = "#f39c12", "Re-enrolling"
	} else if authState >= 2 {
		healthColor, healthText = "#e74c3c", "Auth revoked"
	}

	var b strings.Builder
	b.WriteString(`<html><head><title>cl0q node — operator dashboard</title>
<meta http-equiv="refresh" content="30">
<style>
body{font-family:system-ui,sans-serif;max-width:900px;margin:2em auto;padding:0 1em;color:#222}
h1{font-size:1.4em}
.health{display:inline-block;width:12px;height:12px;border-radius:50%;margin-right:8px}
.section{margin:1.5em 0;border:1px solid #ddd;border-radius:8px;padding:1em}
.section h2{margin:0 0 .5em;font-size:1.1em;color:#555}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(220px,1fr));gap:.8em}
.card{background:#f8f9fa;border-radius:6px;padding:.7em .9em}
.card .label{font-size:.8em;color:#666}
.card .value{font-size:1.5em;font-weight:600}
.card .desc{font-size:.75em;color:#888}
a{color:#0366d6}
.footer{margin-top:2em;font-size:.8em;color:#888}
</style></head><body>
`)
	fmt.Fprintf(&b, "<h1><span class=\"health\" style=\"background:%s\"></span>cl0q node — %s</h1>\n", healthColor, healthText)

	// Helper to render a metric card.
	card := func(label, name, desc string) {
		fmt.Fprintf(&b, "<div class=\"card\"><div class=\"label\">%s</div><div class=\"value\">%s</div><div class=\"desc\">%s</div></div>\n",
			label, formatNum(get(name)), desc)
	}

	// Contribution
	b.WriteString("<div class=\"section\"><h2>Contribution</h2><div class=\"grid\">\n")
	card("Pages crawled", "crawler_pages_ok_total", "successful fetches")
	card("Rows forwarded", "node_rows_forwarded_total", "sent to ingest")
	card("Domains in DB", "crawler_db_domains", "domain_info rows")
	card("Pages in DB", "crawler_db_pages", "website_info rows")
	b.WriteString("</div></div>\n")

	// Efficiency
	b.WriteString("<div class=\"section\"><h2>Efficiency</h2><div class=\"grid\">\n")
	card("Duplicates skipped", "crawler_pages_duplicates_total", "SimHash near-dupes")
	card("Unchanged (304)", "crawler_pages_unchanged_total", "conditional GET hits")
	card("Conditional GETs", "crawler_conditional_requests_total", "recrawls with validators")
	card("Bytes saved (304)", "crawler_bytes_saved_total", "body bytes skipped")
	card("Sitemaps found", "crawler_sitemaps_found_total", "sitemap.xml parsed")
	b.WriteString("</div></div>\n")

	// Politeness
	b.WriteString("<div class=\"section\"><h2>Politeness</h2><div class=\"grid\">\n")
	card("Breaker opens", "crawler_breaker_opens_total", "ASN backoff triggers")
	card("Groups in cooldown", "crawler_breaker_open_groups", "currently backing off")
	card("Challenges seen", "crawler_pages_challenged_total", "bot challenges")
	card("Traps flagged", "crawler_traps_flagged_total", "suspected link farms")
	b.WriteString("</div></div>\n")

	// Problems
	b.WriteString("<div class=\"section\"><h2>Problems</h2><div class=\"grid\">\n")
	card("Fetch failures", "crawler_pages_failed_total", "fetch errors")
	card("Ingest failures", "node_ingest_failures_total", "forward failures")
	card("Dead-lettered", "node_rows_deadlettered_total", "lost rows (should be 0)")
	card("Watermark lag", "node_watermark_lag_website", "unforwarded website rows")
	b.WriteString("</div></div>\n")

	// Frontier
	b.WriteString("<div class=\"section\"><h2>Frontier</h2><div class=\"grid\">\n")
	card("Uncrawled domains", "crawler_frontier_depth", "domains awaiting crawl")
	card("DB size", "node_db_bytes", "local database bytes")
	b.WriteString("</div></div>\n")

	b.WriteString(`<div class="footer">
<p><a href="/metrics">Prometheus /metrics</a> · <a href="/?format=csv">Export CSV</a> · auto-refreshes every 30s</p>
<p>Operator dashboard — shows node health and contribution, never search data or row contents.</p>
</div></body></html>`)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(b.String()))
}

// ServeCSV writes all metrics as CSV: name,type,value,description.
func (r *Registry) ServeCSV(w http.ResponseWriter) {
	counters, gauges := r.snapshot()
	names := []string{}
	for n := range counters {
		names = append(names, n)
	}
	for n := range gauges {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("metric,type,value,description\n")
	for _, name := range names {
		typ := "gauge"
		var val int64
		if v, ok := counters[name]; ok {
			typ, val = "counter", v
		} else {
			val = gauges[name]
		}
		// Quote description (may contain commas).
		desc := strings.ReplaceAll(r.help[name], `"`, `""`)
		fmt.Fprintf(&b, "%s,%s,%d,\"%s\"\n", name, typ, val, desc)
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\"cl0q-node-metrics.csv\"")
	w.Write([]byte(b.String()))
}

// formatNum renders large numbers with commas.
func formatNum(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(c)
	}
	return out.String()
}

// Serve starts the HTTP server on addr (e.g. "127.0.0.1:9100") in the
// background. / serves the status page, /metrics the Prometheus format.
func (r *Registry) Serve(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", r.ServeStatus)
	mux.HandleFunc("/metrics", r.ServeMetrics)
	go func() {
		// The metrics port is for the operator's own monitoring; it never
		// exposes credentials or row contents, only counts.
		_ = http.ListenAndServe(addr, mux)
	}()
}
