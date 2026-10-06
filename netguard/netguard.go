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
	"sync/atomic"
	"syscall"
)

// ErrNotPublic is wrapped in the dial error when a connection to a non-public address is refused.
var ErrNotPublic = errors.New("refusing to connect to a non-public address")

// allowNonPublic turns the check off. It exists only for tests, which run fake
// servers on 127.0.0.1; no crawler sets it, and there is no flag or environment
// variable for it. Unexported: external packages must use SetAllowNonPublicForTest.
// Atomic for safe concurrent access from parallel tests.
var allowNonPublic atomic.Bool

// SetAllowNonPublicForTest toggles the non-public-address guard. TEST ONLY —
// it exists so integration tests in other packages can run fake servers on
// 127.0.0.1. Do not call in production code; the guard is a security boundary.
func SetAllowNonPublicForTest(v bool) { allowNonPublic.Store(v) }

// GetAllowNonPublicForTest reports the guard state. TEST ONLY — paired with
// SetAllowNonPublicForTest so tests can save and restore.
func GetAllowNonPublicForTest() bool { return allowNonPublic.Load() }

// reservedPrefixes lists ranges that IsGlobalUnicast and IsPrivate don't already
// exclude. It is the single source of truth: init derives the per-family
// (network, mask) pairs and the first-octet / first-hextet lookup tables from
// it, so the fast path in Public can never drift out of sync with the list.
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
	// v4Net/v4Mask and v6NetHi/v6NetLo/v6MaskHi/v6MaskLo are the reserved
	// prefixes as precomputed (network, mask) pairs: an address is inside
	// the prefix iff (addr & mask) == net. This is exactly what
	// netip.Prefix.Contains computes, but without its per-call masking
	// overhead; the scan only runs for "maybe" addresses whose first
	// octet/hextet is marked in the tables below.
	v4Net    []uint32
	v4Mask   []uint32
	v6NetHi  []uint64
	v6NetLo  []uint64
	v6MaskHi []uint64
	v6MaskLo []uint64
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
			mb := p.Masked().Addr().As4()
			v4Net = append(v4Net, uint32(mb[0])<<24|uint32(mb[1])<<16|uint32(mb[2])<<8|uint32(mb[3]))
			v4Mask = append(v4Mask, ^uint32(0)<<(32-p.Bits()))
			lo, hi := firstUnitRange(p.Bits(), 8, uint16(p.Masked().Addr().As4()[0]))
			for o := int(lo); o <= int(hi); o++ {
				firstOctetReserved[o] = true
			}
		} else {
			mb := p.Masked().Addr().As16()
			mhi, mlo := v6Mask(p.Bits())
			v6NetHi = append(v6NetHi, be64(mb[:8]))
			v6NetLo = append(v6NetLo, be64(mb[8:]))
			v6MaskHi = append(v6MaskHi, mhi)
			v6MaskLo = append(v6MaskLo, mlo)
			b := p.Masked().Addr().As16()
			base := uint16(b[0])<<8 | uint16(b[1])
			lo, hi := firstUnitRange(p.Bits(), 16, base)
			for h := int(lo); h <= int(hi); h++ {
				firstHextetReserved[h] = true
			}
		}
	}
}

// v6Mask returns the (high, low) 64-bit halves of a 128-bit mask with the top
// bits set.
func v6Mask(bits int) (uint64, uint64) {
	if bits >= 64 {
		return ^uint64(0), ^uint64(0) << (128 - bits)
	}
	return ^uint64(0) << (64 - bits), 0
}

// be64 reads 8 big-endian bytes as a uint64.
func be64(b []byte) uint64 {
	return uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
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

// v4CompatPrefix96 is the IPv4-compatible prefix ::/96 (RFC 2765): 96 zero
// bits with an IPv4 address in the low 32 bits — e.g. ::127.0.0.1. Like the
// SIIT spelling, Go's Addr.Unmap does not translate it, so it would look
// like a global-unicast IPv6 address and slip through the SSRF guard while
// actually reaching an IPv4 host on an RFC 2765 translator.
var v4CompatPrefix96 = [12]byte{}

// siitEmbeddedV4 reclassifies an SIIT-translated IPv6 address (RFC 6145) or an
// IPv4-compatible IPv6 address (RFC 2765, ::/96) as the IPv4 address embedded
// in its low 32 bits.
func siitEmbeddedV4(a netip.Addr) (netip.Addr, bool) {
	if !a.Is6() {
		return netip.Addr{}, false
	}
	b := a.As16()
	var first12 [12]byte
	copy(first12[:], b[:12])
	if first12 != siitV6Prefix16 && first12 != v4CompatPrefix96 {
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
	// Strip any zone ID first: netip.Prefix.Contains returns false for
	// zoned addresses, which would bypass the entire reserved-prefix scan.
	// (P0: CVE-class SSRF bypass via e.g. 64:ff9b::a9fe:a9fe%eth0)
	a = a.WithZone("")
	if v4, ok := siitEmbeddedV4(a); ok {
		// SIIT-translated (RFC 6145) or IPv4-compatible (RFC 2765, ::/96)
		// IPv6: classify the embedded IPv4.
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
		b := a.As4()
		ip := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		for i := range v4Net {
			if ip&v4Mask[i] == v4Net[i] {
				return false
			}
		}
		return true
	}
	b := a.As16()
	if !firstHextetReserved[uint16(b[0])<<8|uint16(b[1])] {
		return true
	}
	hi := be64(b[:8])
	lo := be64(b[8:])
	for i := range v6NetHi {
		if hi&v6MaskHi[i] == v6NetHi[i] && lo&v6MaskLo[i] == v6NetLo[i] {
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
	if allowNonPublic.Load() {
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
