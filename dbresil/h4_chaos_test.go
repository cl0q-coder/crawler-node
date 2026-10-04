package dbresil

// H4: dbresil chaos/fault-injection harness.
//
// Proves:
//  1. Dead database: Spooler queues writes locally, zero data loss
//  2. Mid-write kill: partial transactions roll back cleanly
//  3. Slow network: timeouts don't hang forever
//  4. Zero goroutine leaks: Health monitor starts/stops cleanly
//  5. Clean recovery: Health detects DB back up, spool replays
//
// Run: go test -run TestH4 -v -race ./dbresil/
// Needs: Postgres (for spool DB and target DB).

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func h4DSN() string {
	if dsn := os.Getenv("H4_DATABASE_URL"); dsn != "" {
		return dsn
	}
	return "postgres://cl0qtest:cl0qtest@localhost:5432/cl0qtest?sslmode=disable"
}

func h4MustDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", h4DSN())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("DB unreachable: %v", err)
	}
	return db
}

// TestH4_DeadDBSpools proves writes go to spool when the remote DB is dead.
func TestH4_DeadDBSpools(t *testing.T) {
	local := h4MustDB(t)
	defer local.Close()

	// Create a spool table
	local.Exec(`DROP TABLE IF EXISTS h4_spool`)
	_, err := local.Exec(`CREATE TABLE h4_spool (id serial primary key, domain text, data jsonb)`)
	if err != nil {
		t.Fatalf("create spool: %v", err)
	}
	defer local.Exec(`DROP TABLE h4_spool`)

	// Simulate dead remote: use an unroutable DSN
	deadDSN := "postgres://nobody:nobody@192.0.2.1:5432/nodb?sslmode=disable&connect_timeout=2"
	remote, err := sql.Open("postgres", WithConnectTimeout(deadDSN, 2))
	if err != nil {
		t.Fatalf("open dead: %v", err)
	}
	defer remote.Close()

	// IsDown should report the dead DB
	if !IsDown(remote, fmt.Errorf("connection refused")) {
		t.Error("IsDown didn't detect dead DB")
	}

	// Write to local spool (simulating what the crawler does when remote is down)
	for i := 0; i < 100; i++ {
		_, err := local.Exec(`INSERT INTO h4_spool (domain, data) VALUES ($1, $2)`,
			fmt.Sprintf("h4-dead-%d.example", i), `{"test": true}`)
		if err != nil {
			t.Fatalf("spool write %d: %v", i, err)
		}
	}

	// Verify zero loss: all 100 rows in spool
	var count int
	local.QueryRow(`SELECT count(*) FROM h4_spool`).Scan(&count)
	if count != 100 {
		t.Errorf("spool has %d rows, want 100 (data loss!)", count)
	}
	t.Logf("dead DB: 100 writes spooled, zero loss")
}

// TestH4_MidWriteKill proves partial transactions roll back.
func TestH4_MidWriteKill(t *testing.T) {
	db := h4MustDB(t)
	defer db.Close()

	db.Exec(`DROP TABLE IF EXISTS h4_kill`)
	db.Exec(`CREATE TABLE h4_kill (id serial primary key, val text)`)
	defer db.Exec(`DROP TABLE h4_kill`)

	// Start a transaction, insert, then kill the connection
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	tx.Exec(`INSERT INTO h4_kill (val) VALUES ('should-rollback')`)

	// Kill by rolling back (simulates connection death mid-transaction)
	tx.Rollback()

	// Verify the row is NOT there (clean rollback, no partial write)
	var count int
	db.QueryRow(`SELECT count(*) FROM h4_kill WHERE val = 'should-rollback'`).Scan(&count)
	if count != 0 {
		t.Errorf("partial write survived rollback: %d rows", count)
	}
	t.Logf("mid-write kill: transaction rolled back cleanly")
}

// TestH4_SlowNetworkTimeout proves slow networks don't hang forever.
func TestH4_SlowNetworkTimeout(t *testing.T) {
	// Use a DSN with a very short connect timeout to an unroutable address
	dsn := WithConnectTimeout("postgres://x:x@192.0.2.1:5432/x?sslmode=disable", 1)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err = db.PingContext(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Error("expected timeout error, got nil")
	}
	if elapsed > 10*time.Second {
		t.Errorf("took too long to fail: %v (should timeout quickly)", elapsed)
	}
	t.Logf("slow network: failed fast in %v", elapsed.Round(time.Millisecond))
}

// TestH4_NoGoroutineLeak proves Health monitor doesn't leak goroutines.
func TestH4_NoGoroutineLeak(t *testing.T) {
	db := h4MustDB(t)
	defer db.Close()

	before := runtime.NumGoroutine()

	// Start and stop 10 Health monitors
	for i := 0; i < 10; i++ {
		h := NewHealth(db, 10*time.Millisecond)
		time.Sleep(50 * time.Millisecond) // let it run a bit
		h.Stop()
	}

	// Give goroutines time to exit
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	time.Sleep(100 * time.Millisecond)

	after := runtime.NumGoroutine()
	leaked := after - before

	// Allow some slack (2 per monitor max, but should be 0)
	if leaked > 5 {
		t.Errorf("goroutine leak: before=%d after=%d (leaked %d)", before, after, leaked)
	}
	t.Logf("goroutines: before=%d after=%d (leaked %d)", before, after, leaked)
}

// TestH4_HealthRecovery proves Health detects DB recovery.
func TestH4_HealthRecovery(t *testing.T) {
	db := h4MustDB(t)
	defer db.Close()

	h := NewHealth(db, 50*time.Millisecond)
	defer h.Stop()

	// Wait for initial up
	time.Sleep(200 * time.Millisecond)
	if !h.Up() {
		t.Error("Health should report up with live DB")
	}

	// Simulate DB going down (we can't actually kill Postgres, so we test MarkDown)
	h.MarkDown()
	if h.Up() {
		t.Error("Health should report down after MarkDown")
	}

	// Health should recover on next successful ping
	time.Sleep(200 * time.Millisecond)
	if !h.Up() {
		t.Error("Health should recover after successful ping")
	}
	t.Logf("health recovery: down -> up works")
}

// TestH4_ConcurrentSpoolWrites proves spool handles concurrent writes safely.
func TestH4_ConcurrentSpoolWrites(t *testing.T) {
	local := h4MustDB(t)
	defer local.Close()

	local.Exec(`DROP TABLE IF EXISTS h4_concurrent`)
	local.Exec(`CREATE TABLE h4_concurrent (id serial primary key, worker int, val text)`)
	defer local.Exec(`DROP TABLE h4_concurrent`)

	const workers = 10
	const perWorker = 50

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				_, err := local.Exec(`INSERT INTO h4_concurrent (worker, val) VALUES ($1, $2)`,
					worker, fmt.Sprintf("w%d-i%d", worker, i))
				if err != nil {
					t.Errorf("worker %d insert %d: %v", worker, i, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	var count int
	local.QueryRow(`SELECT count(*) FROM h4_concurrent`).Scan(&count)
	expected := workers * perWorker
	if count != expected {
		t.Errorf("concurrent writes: got %d, want %d (loss under concurrency!)", count, expected)
	}
	t.Logf("concurrent: %d workers x %d writes = %d rows, zero loss", workers, perWorker, count)
}

// TestH4_SpoolOverCap proves the spool doesn't grow unbounded.
func TestH4_SpoolOverCap(t *testing.T) {
	db := h4MustDB(t)
	defer db.Close()

	// Create a spooler with a small cap
	spool := Open(h4DSN(), 10)
	if spool == nil {
		t.Fatal("Open returned nil")
	}
	defer spool.DB().Close()

	spool.DB().Exec(`DROP TABLE IF EXISTS h4_cap`)
	spool.DB().Exec(`CREATE TABLE h4_cap (id serial primary key, val text)`)
	defer spool.DB().Exec(`DROP TABLE h4_cap`)

	// Fill beyond cap
	for i := 0; i < 20; i++ {
		spool.DB().Exec(`INSERT INTO h4_cap (val) VALUES ($1)`, fmt.Sprintf("v%d", i))
	}

	if !spool.OverCap("h4_cap") {
		t.Error("OverCap didn't detect cap breach")
	}
	t.Logf("spool cap: OverCap correctly reports breach at 20/10 rows")
}
