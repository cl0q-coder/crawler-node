// Command preflight runs the crawler-node self-check (NR4).
//
// Operators run this before going live:
//
//	docker compose run --rm preflight
//
// It verifies: the local database is reachable, the ingest API is reachable
// and the token is valid, DNS resolves, egress works, netguard refuses
// non-public addresses, and robots.txt can be fetched and parsed. Each
// check prints a clear PASS/FAIL line; the exit code is 0 only if every
// check passes.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"

	"cl0q/internal/config"
	"cl0q/internal/version"
	"cl0q/netguard"
	"cl0q/robotstxt"
)

type checkResult struct {
	name   string
	pass   bool
	detail string
}

func main() {
	for _, a := range os.Args[1:] {
		if a == "-version" || a == "--version" {
			fmt.Println("preflight", version.Version)
			return
		}
	}
	config.SetMemoryLimitDefault(512 << 20) // preflight is light; 512MiB is plenty

	results := []checkResult{
		checkLocalDB(),
		checkIngest(),
		checkDNS(),
		checkEgress(),
		checkNetguard(),
		checkRobots(),
	}

	failed := 0
	for _, r := range results {
		status := "PASS"
		if !r.pass {
			status = "FAIL"
			failed++
		}
		fmt.Printf("%s: %-10s — %s\n", status, r.name, r.detail)
	}
	if failed > 0 {
		fmt.Printf("\npreflight: %d/%d checks failed\n", failed, len(results))
		os.Exit(1)
	}
	fmt.Printf("\npreflight: all %d checks passed\n", len(results))
}

// guardedClient returns the standard 15s-timeout check client with the
// netguard SSRF guard wired into its transport: dials to non-public
// addresses (loopback, RFC 1918, link-local, metadata, …) are refused.
// Timeouts and all other behavior are unchanged — only the dial Control
// hook is added.
func guardedClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Control: netguard.Control}).DialContext,
		},
	}
}

// checkLocalDB pings the node's Postgres.
func checkLocalDB() checkResult {
	dsn := os.Getenv("LOCAL_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		return checkResult{"local-db", false, "LOCAL_DATABASE_URL / DATABASE_URL not set"}
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return checkResult{"local-db", false, fmt.Sprintf("open: %v", err)}
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return checkResult{"local-db", false, fmt.Sprintf("ping: %v", err)}
	}
	return checkResult{"local-db", true, "Postgres reachable"}
}

// checkIngest verifies the ingest API is reachable and the token is valid
// via the /api/ingest/ping endpoint (see docs/CRAWLER_NODES.md).
func checkIngest() checkResult {
	ingestURL := strings.TrimRight(os.Getenv("INGEST_URL"), "/")
	token := os.Getenv("NODE_TOKEN")
	if ingestURL == "" || token == "" {
		return checkResult{"ingest", false, "INGEST_URL / NODE_TOKEN not set"}
	}
	u, err := url.Parse(ingestURL)
	if err != nil || u.Host == "" {
		return checkResult{"ingest", false, fmt.Sprintf("bad INGEST_URL %q", ingestURL)}
	}
	req, err := http.NewRequest(http.MethodGet, ingestURL+"/api/ingest/ping", nil)
	if err != nil {
		return checkResult{"ingest", false, fmt.Sprintf("build request: %v", err)}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := guardedClient()
	resp, err := client.Do(req)
	if err != nil {
		return checkResult{"ingest", false, fmt.Sprintf("GET %s/api/ingest/ping: %v", ingestURL, err)}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return checkResult{"ingest", false, fmt.Sprintf("ping returned HTTP %d (token or source IP rejected?)", resp.StatusCode)}
	}
	return checkResult{"ingest", true, "ingest reachable, token accepted"}
}

// checkDNS resolves a well-known name through the system resolver.
func checkDNS() checkResult {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, "example.com")
	if err != nil {
		return checkResult{"dns", false, fmt.Sprintf("resolve example.com: %v", err)}
	}
	if len(addrs) == 0 {
		return checkResult{"dns", false, "example.com resolved to no addresses"}
	}
	return checkResult{"dns", true, fmt.Sprintf("example.com -> %s", addrs[0])}
}

// checkEgress performs a plain HTTPS GET to confirm outbound web access.
func checkEgress() checkResult {
	client := guardedClient()
	resp, err := client.Get("https://example.com/")
	if err != nil {
		return checkResult{"egress", false, fmt.Sprintf("GET https://example.com/: %v", err)}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return checkResult{"egress", false, fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return checkResult{"egress", true, "HTTPS egress works"}
}

// checkNetguard verifies the SSRF guard refuses non-public addresses and
// allows public ones — without touching the network.
func checkNetguard() checkResult {
	if netguard.PublicIP("10.0.0.1") {
		return checkResult{"netguard", false, "10.0.0.1 classified public — guard NOT active"}
	}
	if netguard.PublicIP("169.254.169.254") {
		return checkResult{"netguard", false, "169.254.169.254 (cloud metadata) classified public — guard NOT active"}
	}
	if !netguard.PublicIP("8.8.8.8") {
		return checkResult{"netguard", false, "8.8.8.8 classified non-public — guard misconfigured"}
	}
	return checkResult{"netguard", true, "private/link-local/metadata refused, public allowed"}
}

// checkRobots fetches and parses a real robots.txt, proving the robots
// pipeline (fetch + parse) works end to end.
func checkRobots() checkResult {
	client := guardedClient()
	resp, err := client.Get("https://example.com/robots.txt")
	if err != nil {
		return checkResult{"robots", false, fmt.Sprintf("fetch robots.txt: %v", err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return checkResult{"robots", false, fmt.Sprintf("read robots.txt: %v", err)}
	}
	rules := robotstxt.Parse(string(body))
	u, _ := url.Parse("https://example.com/")
	if !rules.Allows(u) {
		return checkResult{"robots", false, "parsed rules disallow https://example.com/ (unexpected)"}
	}
	return checkResult{"robots", true, "robots.txt fetched and parsed"}
}
