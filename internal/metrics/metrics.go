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

// ServeStatus writes a human-readable HTML status page.
func (r *Registry) ServeStatus(w http.ResponseWriter, _ *http.Request) {
	counters, gauges := r.snapshot()
	names := []string{}
	for n := range counters {
		names = append(names, "counter:"+n)
	}
	for n := range gauges {
		names = append(names, "gauge:"+n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("<html><head><title>cl0q node status</title></head><body>\n<h1>cl0q node status</h1>\n<table border=1 cellpadding=4>\n<tr><th>metric</th><th>type</th><th>value</th><th>description</th></tr>\n")
	for _, key := range names {
		typ, name, _ := strings.Cut(key, ":")
		var val int64
		if typ == "counter" {
			val = counters[name]
		} else {
			val = gauges[name]
		}
		fmt.Fprintf(&b, "<tr><td>%s</td><td>%s</td><td>%d</td><td>%s</td></tr>\n",
			name, typ, val, r.help[name])
	}
	b.WriteString("</table>\n<p><a href=\"/metrics\">prometheus /metrics</a></p>\n</body></html>")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(b.String()))
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
