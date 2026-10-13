// Command fixture-server runs the #274 bench fixture (see package fixture)
// as a standalone process, printing the port it listens on:
//
//	fixture-server -n 1000 -latency 2ms
//	PORT=41234
//
// Keeping the server in its own process keeps client CPU profiles honest:
// an in-process httptest server's TLS/HTTP work would otherwise pollute the
// crawler's pprof numbers.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cl0q/tools/bench-fixture"
)

func main() {
	n := flag.Int("n", 1000, "number of fixture sites")
	latency := flag.Duration("latency", 2*time.Millisecond, "artificial per-response server latency")
	flag.Parse()

	srv, err := fixture.Start(*n, *latency)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixture-server:", err)
		os.Exit(1)
	}
	defer srv.Close()
	fmt.Printf("PORT=%s\n", srv.Port())

	// Serve until killed.
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
}
