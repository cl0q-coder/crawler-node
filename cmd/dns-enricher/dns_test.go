package main

import (
	"errors"
	"net"
	"testing"
)

// Only a real answer or NXDOMAIN may overwrite a stored CNAME/MX with "none"; a timeout must not.
func TestDNSConclusive(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, true},
		{&net.DNSError{Err: "no such host", IsNotFound: true}, true},
		{&net.DNSError{Err: "i/o timeout", IsTimeout: true}, false},
		{&net.DNSError{Err: "server misbehaving", IsTemporary: true}, false},
		{errors.New("context deadline exceeded"), false},
	}
	for i, c := range cases {
		if got := dnsConclusive(c.err); got != c.want {
			t.Errorf("case %d (%v): got %v, want %v", i, c.err, got, c.want)
		}
	}
}
