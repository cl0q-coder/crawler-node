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
	"net/netip"
	"syscall"
)

// ErrNotPublic is wrapped in the dial error when a connection to a non-public address is refused.
var ErrNotPublic = errors.New("refusing to connect to a non-public address")

// allowNonPublic turns the check off. It exists only for tests, which run fake
// servers on 127.0.0.1; no crawler sets it, and there is no flag or environment
// variable for it. Unexported: external packages must use SetAllowNonPublicForTest.
var allowNonPublic bool

// SetAllowNonPublicForTest toggles the non-public-address guard. TEST ONLY —
// it exists so integration tests in other packages can run fake servers on
// 127.0.0.1. Do not call in production code; the guard is a security boundary.
func SetAllowNonPublicForTest(v bool) { allowNonPublic = v }

// GetAllowNonPublicForTest reports the guard state. TEST ONLY — paired with
// SetAllowNonPublicForTest so tests can save and restore.
func GetAllowNonPublicForTest() bool { return allowNonPublic }

// reservedPrefixes lists ranges that IsGlobalUnicast and IsPrivate don't already
// exclude. It is the single source of truth: init derives the per-family prefix
// lists and the first-octet / first-hextet lookup tables from it, so the fast
// path in Public can never drift out of sync with the list.
var reservedPrefixes = []string{
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
	"100::/64",        // discard-only (IANA: not globally reachable)
	"100:0:0:1::/64",  // dummy prefix, RFC 9780 (IANA: not globally reachable)
	"2001:2::/48",     // benchmarking (IANA: not globally reachable)
	"3fff::/20",       // documentation (IANA: not globally reachable)
	"5f00::/16",       // SRv6 SIDs (IANA: not globally reachable)
	"192.88.99.2/32",  // 6to4 relay anycast (IANA: not globally reachable)
}

var (
	reserved4 []netip.Prefix
	reserved6 []netip.Prefix
	// firstOctetReserved marks IPv4 first octets covered by at least one
	// reserved prefix; firstHextetReserved marks IPv6 first hextets. An address
	// whose first octet/hextet is unmarked cannot be inside any reserved range,
	// so Public returns true without scanning any prefix.
	firstOctetReserved  [256]bool
	firstHextetReserved [65536]bool
)

func init() {
	for _, s := range reservedPrefixes {
		p := netip.MustParsePrefix(s)
		if p.Addr().Is4() {
			reserved4 = append(reserved4, p)
			lo, hi := firstUnitRange(p.Bits(), 8, uint16(p.Masked().Addr().As4()[0]))
			for o := int(lo); o <= int(hi); o++ {
				firstOctetReserved[o] = true
			}
		} else {
			reserved6 = append(reserved6, p)
			b := p.Masked().Addr().As16()
			base := uint16(b[0])<<8 | uint16(b[1])
			lo, hi := firstUnitRange(p.Bits(), 16, base)
			for h := int(lo); h <= int(hi); h++ {
				firstHextetReserved[h] = true
			}
		}
	}
}

// firstUnitRange returns the range of first-octet (width 8) or first-hextet
// (width 16) values a prefix of the given bit length can cover. base is the
// masked prefix's first octet/hextet; a prefix narrower than the unit spans
// every value its leading bits leave free.
func firstUnitRange(bits, width int, base uint16) (uint16, uint16) {
	if bits >= width {
		return base, base
	}
	span := uint16(1) << (width - bits)
	return base, base + span - 1
}

// siitV6Prefix16 is the first 96 bits of an SIIT-translated IPv6 address
// (RFC 6145): 64 zero bits, then ffff, then 16 zero bits. The low 32 bits
// encode an IPv4 address — e.g. ::ffff:0:127.0.0.1 — but unlike the
// IPv4-mapped spelling (::ffff:a.b.c.d) Go's Addr.Unmap does not translate
// it, so it looks like a global-unicast IPv6 address and would slip through
// the SSRF guard while actually reaching an IPv4 host.
var siitV6Prefix16 = [12]byte{0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 0, 0}

// siitEmbeddedV4 reclassifies an SIIT-translated IPv6 address as the IPv4
// address embedded in its low 32 bits.
func siitEmbeddedV4(a netip.Addr) (netip.Addr, bool) {
	if !a.Is6() {
		return netip.Addr{}, false
	}
	b := a.As16()
	var first12 [12]byte
	copy(first12[:], b[:12])
	if first12 != siitV6Prefix16 {
		return netip.Addr{}, false
	}
	return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
}

// Public reports whether a is a public unicast address a crawler may connect to.
//
// The common case — an ordinary public address — short-circuits after a single
// table lookup: only addresses whose first octet (IPv4) or first hextet (IPv6)
// falls inside a reserved range pay for a prefix scan, and then only against
// the prefixes of their own address family. The tables are derived from
// reservedPrefixes at init, so the fast path and the list cannot disagree;
// every "maybe" address still gets the full scan, so verdicts are unchanged.
func Public(a netip.Addr) bool {
	if v4, ok := siitEmbeddedV4(a.WithZone("")); ok {
		// SIIT-translated IPv6 (RFC 6145): classify the embedded IPv4.
		a = v4
	} else {
		a = a.Unmap()
	}
	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	if a.Is4() {
		if !firstOctetReserved[a.As4()[0]] {
			return true
		}
		for _, p := range reserved4 {
			if p.Contains(a) {
				return false
			}
		}
		return true
	}
	b := a.As16()
	if !firstHextetReserved[uint16(b[0])<<8|uint16(b[1])] {
		return true
	}
	for _, p := range reserved6 {
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
// resolved "ip:port" of each connection attempt. The address is parsed with netip.ParseAddrPort — one allocation-free
// parse — rather than SplitHostPort followed by ParseAddr.
func Control(network, address string, _ syscall.RawConn) error {
	if allowNonPublic {
		return nil
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: bad address %q: %v", ErrNotPublic, address, err)
	}
	a := ap.Addr()
	if !Public(a) {
		return fmt.Errorf("%w: %s", ErrNotPublic, a.Unmap())
	}
	return nil
}
