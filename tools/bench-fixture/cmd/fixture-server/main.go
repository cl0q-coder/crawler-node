// Command fixture-server runs the #274 bench fixture (see package fixture)
// as a standalone process, printing the port it listens on:
//
//	fixture-server -n 1000 -latency 2ms
//	PORT=41234
//
//	# with a custom TLS cert (SANs covering bench{i}.test), for external
//	# comparison crawlers that validate the chain:
//	fixture-server -tls-cert server.pem -tls-key server-key.pem
//
// Keeping the server in its own process keeps client CPU profiles honest:
// an in-process httptest server's TLS/HTTP work would otherwise pollute the
// crawler's pprof numbers.
package main

import (
	"crypto/tls"
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
	tlsCert := flag.String("tls-cert", "", "PEM server certificate (SANs must cover bench{i}.test); empty = httptest generated cert")
	tlsKey := flag.String("tls-key", "", "PEM private key for -tls-cert")
	flag.Parse()

	start := func() (*fixture.Server, error) {
		if *tlsCert == "" {
			return fixture.Start(*n, *latency)
		}
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			return nil, fmt.Errorf("load cert: %w", err)
		}
		return fixture.StartWithCert(*n, *latency, cert)
	}

	srv, err := start()
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
