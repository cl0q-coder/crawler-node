// Command fixturelog runs the #274 bench fixture (copy of
// cl0q/tools/bench-fixture with per-request access logging added) as a
// standalone process. Local-only harness helper for the Nutch leg: lets us
// measure per-host robots.txt -> page gaps from the server side.
//
// Usage:
//
//	fixturelog -n 1000 -latency 2ms -tls-cert server-cert.pem \
//	    -tls-key server-key.pem -log requests.log
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	fixture "fixturelog/fixture"
)

func main() {
	n := flag.Int("n", 1000, "number of fixture sites")
	latency := flag.Duration("latency", 2*time.Millisecond, "artificial per-response server latency")
	tlsCert := flag.String("tls-cert", "", "PEM server certificate")
	tlsKey := flag.String("tls-key", "", "PEM private key for -tls-cert")
	logPath := flag.String("log", "", "request log path (empty = no logging)")
	flag.Parse()

	if *logPath != "" {
		f, err := os.Create(*logPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open log:", err)
			os.Exit(1)
		}
		defer f.Close()
		fixture.RequestLog = log.New(f, "", 0)
	}

	var srv *fixture.Server
	if *tlsCert != "" {
		c, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			fmt.Fprintln(os.Stderr, "load cert:", err)
			os.Exit(1)
		}
		srv, err = fixture.StartWithCert(*n, *latency, c)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fixturelog:", err)
			os.Exit(1)
		}
	} else {
		var err error
		srv, err = fixture.Start(*n, *latency)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fixturelog:", err)
			os.Exit(1)
		}
	}
	defer srv.Close()
	fmt.Printf("PORT=%s\n", srv.Port())

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
}
