package dbresil

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestWithConnectTimeout(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"postgres://u:p@h:5434/db?sslmode=disable", "postgres://u:p@h:5434/db?sslmode=disable&connect_timeout=10"},
		{"postgres://u:p@h/db", "postgres://u:p@h/db?connect_timeout=10"},
		{"postgresql://h/db?connect_timeout=3", "postgresql://h/db?connect_timeout=3"}, // an explicit value wins
		{"host=/var/run/postgresql dbname=cl0q_spool", "host=/var/run/postgresql dbname=cl0q_spool connect_timeout=10"},
		{"", ""},
	} {
		if got := WithConnectTimeout(c.in, 10); got != c.want {
			t.Errorf("WithConnectTimeout(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A shutdown during an outage must not leave a service stuck in WaitForRemote.
func TestWaitForRemoteCtxStops(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://u:p@127.0.0.1:1/x?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := WaitForRemoteCtx(ctx, db); err == nil {
		t.Fatal("want ctx error while the DB is unreachable")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("returned after %v; it should stop when ctx ends", d)
	}
}

func TestHealthStop(t *testing.T) {
	db, _ := sql.Open("postgres", "postgres://u:p@127.0.0.1:1/x?sslmode=disable&connect_timeout=1")
	defer db.Close()
	h := NewHealth(db, 10*time.Millisecond)
	h.Stop()
	h.Stop() // idempotent
	if h.Up() {
		t.Error("an unreachable DB must read as down")
	}
}

func TestSleepCtx(t *testing.T) {
	if !SleepCtx(context.Background(), time.Millisecond) {
		t.Error("full sleep should report true")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if SleepCtx(ctx, time.Hour) {
		t.Error("cancelled sleep should report false")
	}
}
