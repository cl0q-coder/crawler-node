package netguard

import (
	"net/netip"
	"testing"
)

// TestIANASpecialPurpose verifies that every address block the IANA
// special-purpose registries mark as NOT globally reachable is refused by
// Public().
//
// Source (retrieved 2026-10-04, embedded — tests are hermetic and never fetch
// at test time):
//   - https://www.iana.org/assignments/iana-ipv4-special-registry/iana-ipv4-special-registry-1.csv
//   - https://www.iana.org/assignments/iana-ipv6-special-registry/iana-ipv6-special-registry-1.csv
//
// "Globally Reachable = False" rows are listed below. Rows where the registry
// gives no value (e.g. deprecated 192.88.99.0/24, deprecated 2001:10::/28) are
// not asserted on.
//
// Deliberate divergences from a literal reading of the registries:
//   - ::ffff:0:0/96 ("IPv4-mapped", Globally Reachable False): netguard does
//     NOT blanket-refuse it. The low 32 bits name an IPv4 address, and the
//     embedded address's own classification decides (see
//     TestPublicSIITTranslated): ::ffff:0:127.0.0.1 is refused,
//     ::ffff:0:1.1.1.1 is allowed.
//   - 2001::/23 (IETF Protocol Assignments, Globally Reachable False): the
//     block has carved-out globally-reachable sub-assignments (2001:1::1/128,
//     2001:1::2/128, 2001:3::/32, 2001:4:112::/48), so it is covered here at
//     sub-block granularity (2001::/32 Teredo and 2001:2::/48 benchmarking).
//   - 192.0.0.9/32 and 192.0.0.10/32 are marked globally reachable, but sit
//     inside 192.0.0.0/24 (IETF Protocol Assignments, not globally reachable),
//     which netguard refuses wholesale as a conservative choice.
func TestIANASpecialPurpose(t *testing.T) {
	// Every (prefix, sample addresses) pair below comes from a
	// "Globally Reachable = False" row of the IANA registries.
	refused := []struct {
		prefix string
		addrs  []string
	}{
		{"0.0.0.0/8", []string{"0.0.0.0", "0.1.2.3"}},
		{"10.0.0.0/8", []string{"10.0.0.0", "10.1.2.3", "10.255.255.255"}},
		{"100.64.0.0/10", []string{"100.64.0.1", "100.127.255.255"}},
		{"127.0.0.0/8", []string{"127.0.0.1", "127.255.255.255"}},
		{"169.254.0.0/16", []string{"169.254.0.1", "169.254.169.254"}},
		{"172.16.0.0/12", []string{"172.16.0.1", "172.31.255.255"}},
		{"192.0.0.0/24", []string{"192.0.0.1", "192.0.0.255"}},
		{"192.0.0.0/29", []string{"192.0.0.1"}},
		{"192.0.0.8/32", []string{"192.0.0.8"}},
		{"192.0.0.170/32", []string{"192.0.0.170"}},
		{"192.0.0.171/32", []string{"192.0.0.171"}},
		{"192.0.2.0/24", []string{"192.0.2.1"}},
		{"192.88.99.2/32", []string{"192.88.99.2"}},
		{"192.168.0.0/16", []string{"192.168.0.1", "192.168.255.255"}},
		{"198.18.0.0/15", []string{"198.18.0.1", "198.19.255.255"}},
		{"198.51.100.0/24", []string{"198.51.100.7"}},
		{"203.0.113.0/24", []string{"203.0.113.9"}},
		{"240.0.0.0/4", []string{"240.0.0.1", "255.255.255.255"}},
		{"::1/128", []string{"::1"}},
		{"::/128", []string{"::"}},
		{"64:ff9b:1::/48", []string{"64:ff9b:1::1"}},
		{"100::/64", []string{"100::1", "100::ffff"}},
		{"100:0:0:1::/64", []string{"100:0:0:1::1"}},
		{"2001::/32", []string{"2001::1", "2001:0:4136:e378::1"}}, // Teredo
		{"2001:2::/48", []string{"2001:2::1"}},
		{"2001:db8::/32", []string{"2001:db8::1"}},
		{"3fff::/20", []string{"3fff::1", "3fff:fff::1"}},
		{"5f00::/16", []string{"5f00::1", "5f00:ffff::1"}},
		{"fc00::/7", []string{"fc00::1", "fd12:3456::1"}},
		{"fe80::/10", []string{"fe80::1", "febf:ffff::1"}},
	}
	for _, tc := range refused {
		p := netip.MustParsePrefix(tc.prefix)
		for _, s := range tc.addrs {
			a := netip.MustParseAddr(s)
			if !p.Contains(a) {
				t.Fatalf("test bug: %s is not in %s", s, tc.prefix)
			}
			if Public(a) {
				t.Errorf("%s (%s, IANA: not globally reachable) must be refused", s, tc.prefix)
			}
		}
	}

	// Sanity: globally-reachable addresses, including ones the registries
	// explicitly mark Globally Reachable = True, must stay allowed.
	allowed := []string{
		"1.1.1.1", "8.8.8.8", "93.184.215.14",
		"2606:4700:4700::1111",
		"192.31.196.1",     // AS112-v4, IANA Globally Reachable True
		"192.52.193.1",     // AMT, IANA Globally Reachable True
		"192.175.48.1",     // Direct Delegation AS112, IANA Globally Reachable True
		"2001:1::1",        // PCP anycast, IANA Globally Reachable True
		"2001:1::2",        // TURN anycast, IANA Globally Reachable True
		"2001:3::1",        // AMT, IANA Globally Reachable True
		"2001:4:112::1",    // AS112-v6, IANA Globally Reachable True
		"::ffff:0:1.1.1.1", // SIIT spelling of a public IPv4 address
	}
	for _, s := range allowed {
		if !Public(netip.MustParseAddr(s)) {
			t.Errorf("%s must be allowed", s)
		}
	}
}
