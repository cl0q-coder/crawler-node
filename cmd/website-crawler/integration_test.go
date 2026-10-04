//go:build integration

// C5/C6 integration harness.
//
// NEEDS SERVER RUN. Plain `go test ./...` skips this file (build tag).
// Requires Postgres 18 with crawler-node initdb/01-schema.sql applied:
//
//	DATABASE_URL=postgres://cl0q:cl0q@localhost:5432/cl0q?sslmode=disable \
//	  go test -tags integration ./cmd/website-crawler/ -run 'TestIntegration|TestNoLostBatch' -v
//
// C5: fake-site → fetch → DB write → read-back (the crawl pipeline's shape
// without the network or the ingest hop). C6: no-lost-batch — flushBatch takes
// no context; its write deadline derives from context.Background, so a
// SIGTERM/SIGINT (which cancels main's ctx) cannot abort an in-flight batch.
// The fake-ingest hop (cl0q-node → cl0q.com) is intentionally out of scope:
// it needs enrollment credentials and is covered by docs/CRAWLER_NODES.md.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — integration test needs Postgres 18 with initdb/01-schema.sql (see header)")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("Postgres unreachable at DATABASE_URL: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func cleanDomains(t *testing.T, db *sql.DB, like string) {
	t.Helper()
	if _, err := db.Exec(`DELETE FROM website_info WHERE domain LIKE $1`, like); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

// fakeSite serves one deterministic page. The address guard is off in tests
// (netguard.AllowNonPublic = true in etiquette_test.go's init), so the crawler
// can fetch 127.0.0.1 directly.
func fakeSite(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>Fake Test Site</title>`+
			`<meta name="description" content="A deterministic fake page for the integration harness.">`+
			`</head><body><h1>Fake Test Site</h1>`+
			`<p>Hello from the fake site. It links <a href="http://other.invalid/">elsewhere</a>.</p>`+
			`</body></html>`)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

// C5: fake site → fetchWebsiteInfo → flushBatchTx → read back from Postgres.
// Exercises the real fetch/parse/persist path against a deterministic page.
func TestIntegrationFakeSiteToDB(t *testing.T) {
	db := testDB(t)
	cleanDomains(t, db, "%.integration")
	defer cleanDomains(t, db, "%.integration")

	addr := fakeSite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	info, err := fetchWebsiteInfo(ctx, addr, "", "", "", nil)
	if err != nil {
		t.Fatalf("fetchWebsiteInfo(%s): %v", addr, err)
	}
	if info == nil {
		t.Fatal("fetchWebsiteInfo returned nil info, nil error")
	}
	if !strings.Contains(info.Title, "Fake Test Site") {
		t.Errorf("title = %q, want it to contain %q", info.Title, "Fake Test Site")
	}
	if info.StatusCode != 200 {
		t.Errorf("status = %d, want 200", info.StatusCode)
	}

	batch := []crawlOut{{
		domain: "fake-site.integration",
		info:   *info,
	}}
	// flushBatchTx returns the discovery-emit count (0 here: emit off); the
	// write itself is verified by the read-back below.
	if _, err := flushBatchTx(ctx, db, batch, false, false); err != nil {
		t.Fatalf("flushBatchTx: %v", err)
	}

	var title, contentType string
	var status int
	err = db.QueryRow(`SELECT title, status_code, content_type FROM website_info WHERE domain = 'fake-site.integration'`).
		Scan(&title, &status, &contentType)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(title, "Fake Test Site") {
		t.Errorf("persisted title = %q, want it to contain %q", title, "Fake Test Site")
	}
	if status != 200 {
		t.Errorf("persisted status = %d, want 200", status)
	}
}

// C6: no-lost-batch. A batch handed to flushBatch must land whole: the function
// deliberately takes no context (deadline derives from context.Background), so
// shutdown cancellation cannot strand it. 25 fresh domains in, 25 rows out.
func TestNoLostBatchOnShutdown(t *testing.T) {
	db := testDB(t)
	cleanDomains(t, db, "noloss-%.integration")
	defer cleanDomains(t, db, "noloss-%.integration")

	const n = 25
	batch := make([]crawlOut, 0, n)
	for i := 0; i < n; i++ {
		batch = append(batch, crawlOut{
			domain: fmt.Sprintf("noloss-%d.integration", i),
			info: WebsiteInfo{
				Title:       fmt.Sprintf("No-loss page %d", i),
				Description: "no-lost-batch probe",
				StatusCode:  200,
				ContentType: "text/html",
			},
		})
	}

	// NOTE: flushBatch's signature has no ctx parameter — verified by reading
	// main.go. There is no cancellation handle for a caller (or a shutdown
	// path) to abort the write with. (Its int return is the discovery-emit
	// count, 0 with emit off — the row count below is the real assertion.)
	flushBatch(db, nil, batch, nil, nil, nil, nil, nil)

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM website_info WHERE domain LIKE 'noloss-%.integration'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != n {
		t.Errorf("website_info has %d no-loss rows, want %d — batch lost rows", count, n)
	}
}
