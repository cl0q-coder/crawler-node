// Command `cl0q-node status`: operator snapshot of node health and
// contribution (OP2). Queries the local /metrics endpoint (METRICS_ADDR).
//
// Usage:
//
//	cl0q-node status          # human-readable summary
//	cl0q-node status --json   # JSON for scripting
//	cl0q-node status --csv    # CSV export (OP3)
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// runStatusCommand implements `cl0q-node status`.
func runStatusCommand(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "output as JSON")
	csvOut := fs.Bool("csv", false, "output as CSV")
	_ = fs.Parse(args)

	addr := os.Getenv("METRICS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9100"
	}
	// Normalize: allow ":9100" or "127.0.0.1:9100".
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	url := "http://" + addr + "/metrics"

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "status: cannot reach metrics at %s: %v\n", url, err)
		fmt.Fprintf(os.Stderr, "Is the node running with METRICS_ADDR set?\n")
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "status: read error: %v\n", err)
		os.Exit(1)
	}

	metrics := parsePrometheus(string(body))

	if *jsonOut {
		out, _ := json.MarshalIndent(metrics, "", "  ")
		fmt.Println(string(out))
		return
	}
	if *csvOut {
		w := csv.NewWriter(os.Stdout)
		_ = w.Write([]string{"metric", "value"})
		names := make([]string, 0, len(metrics))
		for n := range metrics {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			_ = w.Write([]string{n, strconv.FormatInt(metrics[n], 10)})
		}
		w.Flush()
		return
	}

	// Human-readable summary.
	printStatusSummary(metrics)
}

// parsePrometheus parses Prometheus text format into name->value.
func parsePrometheus(text string) map[string]int64 {
	out := map[string]int64{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		if v, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
			out[parts[0]] = v
		}
	}
	return out
}

// printStatusSummary renders the operator summary.
func printStatusSummary(m map[string]int64) {
	get := func(name string) int64 { return m[name] }

	// Health
	auth := get("node_auth_state")
	health := "HEALTHY"
	if auth == 1 {
		health = "RE-ENROLLING"
	} else if auth >= 2 {
		health = "AUTH REVOKED"
	}
	fmt.Printf("cl0q node status — %s\n", health)
	fmt.Printf("  Auth state:      %d\n", auth)
	fmt.Println()

	// Contribution
	fmt.Println("Contribution:")
	fmt.Printf("  Pages crawled:   %s\n", formatNumCLI(get("crawler_pages_ok_total")))
	fmt.Printf("  Rows forwarded:  %s\n", formatNumCLI(get("node_rows_forwarded_total")))
	fmt.Printf("  Domains in DB:   %s\n", formatNumCLI(get("crawler_db_domains")))
	fmt.Println()

	// Efficiency
	fmt.Println("Efficiency:")
	fmt.Printf("  Duplicates:      %s (SimHash)\n", formatNumCLI(get("crawler_pages_duplicates_total")))
	fmt.Printf("  Unchanged (304): %s\n", formatNumCLI(get("crawler_pages_unchanged_total")))
	fmt.Printf("  Sitemaps:        %s\n", formatNumCLI(get("crawler_sitemaps_found_total")))
	fmt.Println()

	// Problems
	fmt.Println("Problems:")
	fmt.Printf("  Fetch failures:  %s\n", formatNumCLI(get("crawler_pages_failed_total")))
	fmt.Printf("  Ingest failures: %s\n", formatNumCLI(get("node_ingest_failures_total")))
	fmt.Printf("  Dead-lettered:   %s\n", formatNumCLI(get("node_rows_deadlettered_total")))
	fmt.Printf("  Watermark lag:   %s rows\n", formatNumCLI(get("node_watermark_lag_website")))
}

func formatNumCLI(n int64) string {
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
