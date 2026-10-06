package netguard

import (
	"math/rand"
	"net/netip"
	"testing"
)

// referencePublic recomputes the verdict the slow way: parse the reserved
// list fresh and use netip.Prefix.Contains, mirroring the pre-optimization
// scan. The (network, mask) pairs must agree with it on every address.
func referencePublic(a netip.Addr) bool {
	if v4, ok := siitEmbeddedV4(a.WithZone("")); ok {
		a = v4
	} else {
		a = a.Unmap()
	}
	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, s := range reservedPrefixes {
		if netip.MustParsePrefix(s).Contains(a) {
			return false
		}
	}
	return true
}

func TestScanMatchesPrefixContains(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	// Targeted: every reserved prefix's network, broadcast-ish, and neighbors.
	for _, s := range reservedPrefixes {
		p := netip.MustParsePrefix(s)
		net := p.Masked().Addr()
		addrs := []netip.Addr{net}
		if net.Is4() {
			b := net.As4()
			b[3] ^= 0x01
			addrs = append(addrs, netip.AddrFrom4(b))
			b[3] ^= 0xff
			addrs = append(addrs, netip.AddrFrom4(b))
		}
		for _, a := range addrs {
			if got, want := Public(a), referencePublic(a); got != want {
				t.Errorf("Public(%v) = %v, reference = %v", a, got, want)
			}
		}
		_ = p
	}
	// Random: 200k addresses across both families, incl. 4-in-6 shapes.
	for i := 0; i < 200000; i++ {
		var a netip.Addr
		switch r.Intn(4) {
		case 0, 1:
			var b [4]byte
			r.Read(b[:])
			a = netip.AddrFrom4(b)
		case 2:
			var b [16]byte
			r.Read(b[:])
			a = netip.AddrFrom16(b)
		default:
			var b [4]byte
			r.Read(b[:])
			a = netip.AddrFrom16([16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, b[0], b[1], b[2], b[3]})
		}
		if got, want := Public(a), referencePublic(a); got != want {
			t.Fatalf("Public(%v) = %v, reference = %v", a, got, want)
		}
	}
}
