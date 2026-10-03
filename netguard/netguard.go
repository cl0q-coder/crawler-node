// Package netguard keeps a crawler node on the public internet.
//
// Control is installed as the net.Dialer Control hook of every crawler HTTP transport, so it sees the address a socket
// is actually about to connect to — after DNS resolution, for every redirect hop and every fallback — and refuses
// anything that is not a public unicast address: loopback, private networks (RFC 1918, fc00::/7), link-local
// (169.254.0.0/16, where cloud instance metadata lives, and fe80::/10), carrier-grade NAT (100.64.0.0/10), multicast,
// and the other reserved and documentation ranges.
//
// Without it, a page linking to http://169.254.169.254/ — or to a hostname that resolves to 10.0.0.1 — would make a
// node fetch its own cloud provider's metadata service or its operator's private network and forward what it found.
package netguard

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// ErrNotPublic is wrapped in the dial error when a connection to a non-public address is refused.
var ErrNotPublic = errors.New("refusing to connect to a non-public address")

// AllowNonPublic turns the check off. It exists only for tests, which run fake servers on 127.0.0.1; no crawler sets
// it, and there is no flag or environment variable for it.
var AllowNonPublic bool

// reserved lists ranges that IsGlobalUnicast and IsPrivate don't already exclude.
var reserved = mustPrefixes(
	"0.0.0.0/8",       // "this network"
	"100.64.0.0/10",   // carrier-grade NAT (also used by VPN overlay networks)
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"240.0.0.0/4",     // reserved, incl. 255.255.255.255
	"64:ff9b::/96",    // NAT64: reaches IPv4 (including private IPv4) through a translator
	"64:ff9b:1::/48",  // local-use NAT64
	"2001::/32",       // Teredo
	"2001:db8::/32",   // documentation
	"2002::/16",       // 6to4: embeds an IPv4 address
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// Public reports whether a is a public unicast address a crawler may connect to.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, p := range reserved {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// PublicIP is Public for a textual address; anything that doesn't parse as an IP is not public.
func PublicIP(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && Public(a)
}

// Control is a net.Dialer Control hook that refuses to connect to non-public addresses. The dialer calls it with the
// resolved "ip:port" of each connection attempt.
func Control(network, address string, _ syscall.RawConn) error {
	if AllowNonPublic {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: bad address %q: %v", ErrNotPublic, address, err)
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: %q is not an IP address", ErrNotPublic, host)
	}
	if !Public(a) {
		return fmt.Errorf("%w: %s", ErrNotPublic, a.Unmap())
	}
	return nil
}
