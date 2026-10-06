package netguard

import (
	"net/netip"
	"testing"
)

// benchSinkBool and benchSinkErr keep benchmark results live so the compiler
// cannot eliminate the measured call.
var benchSinkBool bool
var benchSinkErr error

// BenchmarkPublic measures the per-address cost of the SSRF guard across the
// address shapes a crawler dials: plain IPv4, IPv4-mapped IPv6 (the shape DNS
// resolvers commonly hand back), and IPv6 — each in a public (common case) and
// a refused variant. Allocs/op must stay at 0: Public runs on the dial hot
// path, once per connection attempt.
func BenchmarkPublic(b *testing.B) {
	cases := []struct {
		name string
		addr netip.Addr
	}{
		{"IPv4Public", netip.MustParseAddr("1.1.1.1")},
		{"IPv4Private", netip.MustParseAddr("10.0.0.1")},
		{"IPv4Reserved", netip.MustParseAddr("192.0.2.1")},
		{"IPv4MappedPublic", netip.MustParseAddr("::ffff:1.1.1.1")},
		{"IPv4MappedPrivate", netip.MustParseAddr("::ffff:192.168.0.1")},
		{"IPv6Public", netip.MustParseAddr("2606:4700:4700::1111")},
		{"IPv6Documentation", netip.MustParseAddr("2001:db8::1")},
		{"IPv6NAT64", netip.MustParseAddr("64:ff9b::808:808")},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchSinkBool = Public(c.addr)
			}
		})
	}
}

// BenchmarkControl measures the full Control hook — "ip:port" parsing plus the
// Public check — exactly as the dialer invokes it per connection attempt.
// The refused variants allocate (fmt.Errorf); the public fast path must not.
func BenchmarkControl(b *testing.B) {
	allowNonPublic.Store(false)
	cases := []struct {
		name string
		addr string
	}{
		{"IPv4Public", "93.184.215.14:443"},
		{"IPv4Refused", "169.254.169.254:80"},
		{"IPv4MappedPublic", "[::ffff:1.1.1.1]:443"},
		{"IPv6Public", "[2606:4700:4700::1111]:443"},
		{"IPv6Refused", "[::1]:80"},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchSinkErr = Control("tcp", c.addr, nil)
			}
		})
	}
}
