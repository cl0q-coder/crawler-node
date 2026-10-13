// Package fixture is the hermetic crawl-fixture server for #274 (crawler
// optimization + open-source crawler comparison).
//
// It serves a fixed, deterministic set of N "sites": each site is a hostname
// bench{i}.test whose homepage is an HTML page of a fixed size class
// (1KB / 50KB / 500KB mix) plus a small shared /robots.txt. All responses
// carry an artificial, fixed server-side latency so timings are reproducible.
//
// The server listens on 0.0.0.0 (all local interfaces) so that benchmark
// drivers can resolve the N fixture hostnames to N distinct 127/8 addresses
// (exercising the per-IP dial/politeness path) while a single socket accepts
// them all. TLS is provided via httptest.StartTLS; the driver is expected to
// set InsecureSkipVerify on its transport (test-only) since the cert is for
// 127.0.0.1, not the fixture hostnames.
package fixture

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

// Size classes (target homepage body bytes).
const (
	SizeSmall  = 1024       // 1KB
	SizeMedium = 50 * 1024  // 50KB
	SizeLarge  = 500 * 1024 // 500KB
)

// SizeFor returns the deterministic size class for site index i:
// 60% small, 30% medium, 10% large.
func SizeFor(i int) int {
	switch i % 10 {
	case 0, 1, 2, 3, 4, 5:
		return SizeSmall
	case 6, 7, 8:
		return SizeMedium
	default:
		return SizeLarge
	}
}

// Host returns the fixture hostname for site index i.
func Host(i int) string { return fmt.Sprintf("bench%d.test", i) }

// RobotsBody is the fixed robots.txt served for every fixture host: permissive,
// no Crawl-delay (so the driver's own politeness config is what gets measured),
// with enough directives to exercise the robots.txt parse path.
const RobotsBody = `User-agent: *
Allow: /
Disallow: /admin/
Disallow: /private/
Disallow: /tmp/
Disallow: /*.pdf$
Disallow: /search?

User-agent: cl0q-node
Allow: /

User-agent: badbot
Disallow: /

Sitemap: https://bench0.test/sitemap.xml
`

// RequestLog, when non-nil, receives one line per request:
// "<unixnano> <host> <path>".
var RequestLog *log.Logger

// Server is a running fixture instance.
type Server struct {
	srv     *httptest.Server
	Latency time.Duration // artificial per-response server delay
	N       int
}

// URL returns the https base URL (host 127.0.0.1) of the fixture.
func (s *Server) URL() string { return s.srv.URL }

// Port returns the TCP port the fixture listens on.
func (s *Server) Port() string {
	_, port, err := net.SplitHostPort(s.srv.Listener.Addr().String())
	if err != nil {
		panic(err)
	}
	return port
}

// HostURL returns the https URL for site i using its fixture hostname and the
// server's port, e.g. https://bench42.test:PORT/.
func (s *Server) HostURL(i int) string {
	_, port, err := net.SplitHostPort(strings.TrimPrefix(s.srv.URL, "https://"))
	if err != nil {
		panic(err)
	}
	return "https://" + Host(i) + ":" + port + "/"
}

// Close shuts the fixture down.
func (s *Server) Close() { s.srv.Close() }

// pageHTML builds the deterministic homepage for site i at roughly size bytes.
// Real DOM with a title, meta description, a few absolute links (exercising
// link extraction) and lorem-ish text paragraphs to reach the target size.
func pageHTML(i, size int) string {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html><head>")
	fmt.Fprintf(&b, "<title>Fixture page %d — cl0q #274 baseline</title>", i)
	fmt.Fprintf(&b, "<meta name=\"description\" content=\"Deterministic fixture page %d for crawler benchmarking.\">", i)
	fmt.Fprintf(&b, "<link rel=\"canonical\" href=\"https://bench%d.test/\">", i)
	b.WriteString("</head><body>")
	fmt.Fprintf(&b, "<h1>Fixture page %d</h1>", i)
	// A few absolute outlinks so link extraction has something to do.
	for j := 0; j < 3; j++ {
		fmt.Fprintf(&b, "<a href=\"https://bench%d.test/page%d\">neighbour %d</a>", (i+j+1)%1000, j, j)
	}
	para := fmt.Sprintf("<p>Paragraph of deterministic fixture text for page %d. "+
		"The quick brown fox jumps over the lazy dog. Pack my box with five dozen liquor jugs. "+
		"Crawlers measure throughput against fixtures like this one.</p>", i)
	for b.Len() < size {
		b.WriteString(para)
	}
	b.WriteString("</body></html>")
	return b.String()
}

// Start brings up the fixture serving N sites with a fixed per-response
// latency. The returned Server must be Closed by the caller.
func Start(n int, latency time.Duration) (*Server, error) {
	return start(n, latency, nil)
}

// StartWithCert is like Start but serves TLS with the provided certificate
// instead of httptest's generated one. Added for #274 phase 3 so external
// comparison crawlers (Heritrix, Scrapy, …) can validate the fixture TLS
// chain against a CA whose SANs cover the bench{i}.test hostnames.
func StartWithCert(n int, latency time.Duration, cert tls.Certificate) (*Server, error) {
	return start(n, latency, &cert)
}

func start(n int, latency time.Duration, cert *tls.Certificate) (*Server, error) {
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("fixture listen: %w", err)
	}
	s := &Server{Latency: latency, N: n}
	pages := make([]string, n)
	for i := 0; i < n; i++ {
		pages[i] = pageHTML(i, SizeFor(i))
	}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RequestLog != nil {
			RequestLog.Printf("%d %s %s", time.Now().UnixNano(), r.Host, r.URL.Path)
		}
		if s.Latency > 0 {
			time.Sleep(s.Latency)
		}
		if r.URL.Path == "/robots.txt" {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, RobotsBody)
			return
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		idx := -1
		var v int
		if _, err := fmt.Sscanf(host, "bench%d.test", &v); err == nil && v >= 0 && v < n {
			idx = v
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if idx >= 0 {
			fmt.Fprint(w, pages[idx])
			return
		}
		http.NotFound(w, r)
	}))
	s.srv.Listener = ln
	if cert != nil {
		s.srv.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
	}
	s.srv.StartTLS()
	return s, nil
}
