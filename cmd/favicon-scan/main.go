package main

// favicon-scan — #111 v1: favicon brand-impersonation detection (exact-match SHA256).
//
// Batch tool (nightly-job shaped): fetches the favicon of each crawled domain,
// SHA256-hashes it, and exact-matches it against the curated brand fingerprint
// seed (brands/brand_favicons.json). A domain whose favicon bytes are identical
// to a brand's favicon but whose registrable domain is not the brand's is
// recorded as a probable brand impersonator (phishing signal).
//
// v1 measurement: an average-hash (aHash, no new deps) is computed for every
// successfully decoded favicon, and near-matches (Hamming <= -near-threshold)
// that are NOT exact matches are counted as probable re-encoded variants.
// The final report quantifies the re-encoded variant rate, which informs the
// pHash v2 decision. No pHash is built in v1.
//
// Input: -domains file (one domain per line) or -db DSN (reads crawled domains
// from the node-local domain_info and persists hits to favicon_impersonation_hits).
// Output: stdout summary + -out JSONL report of hits.
//
// Server-side companion (cl0q-search): brand_favicons table, /api/impersonators
// feed, backfill-impersonators.sql. This tool is the node-side detection half;
// it does not touch the ingest wire protocol.
//
// Usage:
//	go run ./cmd/favicon-scan -brands brands/brand_favicons.json -domains /tmp/domains.txt -out /tmp/hits.jsonl
//	go run ./cmd/favicon-scan -db "postgres://cl0q:cl0q@localhost:5432/cl0q?sslmode=disable" -limit 5000

import (
	"bufio"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"codeberg.org/cl0qsearch/crawler-node/dbresil"

	_ "github.com/lib/pq"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	brandsPath := flag.String("brands", "brands/brand_favicons.json", "path to brand fingerprint seed JSON")
	domainsFile := flag.String("domains", "", "file with one domain per line (alternative to -db)")
	dsn := flag.String("db", "", "postgres DSN for node-local DB (reads domain_info, writes hits)")
	limit := flag.Int("limit", 10000, "max domains to scan from DB")
	workers := flag.Int("workers", 16, "concurrent fetch workers")
	fetchTimeout := flag.Duration("timeout", 15*time.Second, "per-request timeout for favicon fetches")
	out := flag.String("out", "", "write JSONL hit report to this path (in addition to stdout summary)")
	cdnThreshold := flag.Int("cdn-threshold", 1000, "favicons used by more than this many distinct non-brand domains are excluded as CDN/generic icons")
	nearThreshold := flag.Int("near-threshold", 8, "max aHash Hamming distance for the re-encoded-variant measurement")
	saveDB := flag.Bool("save-db", true, "persist hits to favicon_impersonation_hits when -db is given")
	flag.Parse()

	if *domainsFile == "" && *dsn == "" {
		log.Fatal("need one of -domains or -db")
	}

	brands, err := loadBrands(*brandsPath)
	if err != nil {
		log.Fatalf("load brands: %v", err)
	}
	log.Printf("loaded %d brand fingerprints from %s", len(brands), *brandsPath)

	var domains []string
	var db *sql.DB
	if *dsn != "" {
		db, err = sql.Open("postgres", dbresil.WithConnectTimeout(*dsn, 10))
		if err != nil {
			log.Fatalf("open db: %v", err)
		}
		defer db.Close()
		if err := db.Ping(); err != nil {
			log.Fatalf("ping db: %v", err)
		}
		if err := ensureSchema(db); err != nil {
			log.Fatalf("ensure schema: %v", err)
		}
		domains, err = loadDomains(db, *limit)
		if err != nil {
			log.Fatalf("load domains: %v", err)
		}
	} else {
		domains, err = readDomainsFile(*domainsFile)
		if err != nil {
			log.Fatalf("read domains file: %v", err)
		}
	}
	log.Printf("scanning %d domains with %d workers", len(domains), *workers)

	client := &http.Client{
		Timeout: *fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}

	idx := newBrandIndex(brands)
	sc := &scanner{fetch: newFetcher(client, *fetchTimeout), idx: idx, nearThreshold: *nearThreshold}

	jobs := make(chan string, *workers*2)
	results := make(chan *ScanResult, *workers*2)
	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range jobs {
				results <- sc.scanDomain(d)
			}
		}()
	}
	go func() {
		for _, d := range domains {
			jobs <- d
		}
		close(jobs)
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	var all []*ScanResult
	for r := range results {
		all = append(all, r)
	}

	// Second pass: CDN/generic-icon exclusion (Micah's nit — brands are never
	// excluded by popularity; only the non-brand distinct-domain count counts).
	applyCDNExclusion(all, *cdnThreshold)

	rep := buildReport(all, brands)
	printSummary(rep)

	if *out != "" {
		if err := writeJSONL(*out, rep.Hits); err != nil {
			log.Fatalf("write report: %v", err)
		}
		log.Printf("hit report written to %s", *out)
	}

	if db != nil && *saveDB {
		n, err := saveHits(db, rep.Hits)
		if err != nil {
			log.Fatalf("save hits: %v", err)
		}
		log.Printf("persisted %d hits to favicon_impersonation_hits", n)
	}
}

func readDomainsFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		d := strings.TrimSpace(sc.Text())
		d = strings.TrimPrefix(d, "http://")
		d = strings.TrimPrefix(d, "https://")
		if i := strings.Index(d, "/"); i >= 0 {
			d = d[:i]
		}
		if d != "" {
			out = append(out, d)
		}
	}
	return out, sc.Err()
}
