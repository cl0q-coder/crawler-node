package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"
)

func TestPublic(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", "0.0.0.0", "::", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"fe80::1", "fc00::1", "fd12:3456::1", "100.64.0.1", "100.100.100.100", "192.0.0.8", "192.0.2.1",
		"198.18.0.1", "198.51.100.7", "203.0.113.9", "224.0.0.1", "239.255.255.250", "255.255.255.255",
		"240.0.0.1", "ff02::1", "::ffff:127.0.0.1", "::ffff:169.254.169.254", "64:ff9b::a00:1", "2001:db8::1",
		"2002:a00:1::1", "2001:0:4136:e378::1",
	}
	allowed := []string{"1.1.1.1", "8.8.8.8", "93.184.215.14", "9.9.9.9", "2606:4700:4700::1111", "::ffff:1.1.1.1"}
	for _, s := range blocked {
		if Public(netip.MustParseAddr(s)) {
			t.Errorf("%s must be refused", s)
		}
	}
	for _, s := range allowed {
		if !Public(netip.MustParseAddr(s)) {
			t.Errorf("%s must be allowed", s)
		}
	}
	if PublicIP("not-an-ip") || PublicIP("") {
		t.Error("non-IP strings are not public")
	}
}

// A real HTTP client with Control installed must refuse a loopback server before any byte is sent.
func TestDialRefusesLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- struct{}{}
			c.Close()
		}
	}()
	d := &net.Dialer{Timeout: 2 * time.Second, Control: Control}
	cli := &http.Client{Transport: &http.Transport{DialContext: d.DialContext}}
	_, err = cli.Get("http://" + ln.Addr().String() + "/")
	if !errors.Is(err, ErrNotPublic) {
		t.Fatalf("want ErrNotPublic, got %v", err)
	}
	select {
	case <-accepted:
		t.Fatal("the loopback server must never see a connection")
	case <-time.After(100 * time.Millisecond):
	}
	// localhost resolves to loopback: refused too
	if _, err := (&net.Dialer{Control: Control}).DialContext(context.Background(), "tcp", "localhost:9"); !errors.Is(err, ErrNotPublic) {
		t.Fatalf("localhost must be refused, got %v", err)
	}
}

func TestControlWrapsBadAddress(t *testing.T) {
	AllowNonPublic = false
	if err := Control("tcp", "no-port-here", nil); !errors.Is(err, ErrNotPublic) {
		t.Fatalf("an unparseable address must be refused with ErrNotPublic, got %v", err)
	}
}
