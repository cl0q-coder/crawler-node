// Package netguard_oracle is crawler-node's INDEPENDENT check on its vendored
// SSRF guard (cl0q/netguard). It embeds the IANA special-purpose registries'
// Globally-Reachable flag as ground truth and verifies the vendored guard
// honors it: every prefix IANA marks NOT globally reachable must be refused,
// and addresses IANA marks globally reachable must stay allowed.
//
// This is deliberately maintained separately from the vendored package's own
// tests (netguard/iana_test.go): if the vendored copy ever drifts from the
// canonical standalone netguard repo, this oracle catches the behavioral
// difference. (The N1 vendor-drift CI job catches file-level drift; this
// catches behavior-level drift.)
//
// Ground truth source (retrieved 2026-10-04, embedded so the test is hermetic):
//   - https://www.iana.org/assignments/iana-ipv4-special-registry/iana-ipv4-special-registry-1.csv
//   - https://www.iana.org/assignments/iana-ipv6-special-registry/iana-ipv6-special-registry-1.csv
//
// Deliberately NOT asserted here:
//   - Rows where the registry gives no Globally-Reachable value (deprecated
//     192.88.99.0/24 "6to4 Relay Anycast", deprecated 2001:10::/28 "ORCHID").
//     The guard's stance on those is the canonical netguard repo's call, not
//     this oracle's.
//   - 64:ff9b::/96 (NAT64): IANA marks it globally reachable, but the guard
//     conservatively refuses it because it can reach private IPv4 through a
//     translator. Documented divergence, not an oracle failure.
//   - ::ffff:0:0/96 (IPv4-mapped): classified by the EMBEDDED IPv4 address,
//     not blanket-refused. Covered by TestN2_OracleSIITSpellings below.
package netguard_oracle

import (
	"net/netip"
	"testing"

	"cl0q/netguard"
)

// n2Refused: every (prefix, sample addresses) pair below comes from a
// "Globally Reachable = False" row of the IANA registries.
var n2Refused = []struct {
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
	{"192.88.99.2/32", []string{"192.88.99.2"}}, // 6to4 relay anycast (only the /32, not the deprecated /24)
	{"192.168.0.0/16", []string{"192.168.0.1", "192.168.255.255"}},
	{"198.18.0.0/15", []string{"198.18.0.1", "198.19.255.255"}},
	{"198.51.100.0/24", []string{"198.51.100.7"}},
	{"203.0.113.0/24", []string{"203.0.113.9"}},
	{"240.0.0.0/4", []string{"240.0.0.1", "255.255.255.255"}},
	{"::1/128", []string{"::1"}},
	{"::/128", []string{"::"}},
	{"64:ff9b:1::/48", []string{"64:ff9b:1::1"}},
	{"100::/64", []string{"100::1", "100::ffff"}},      // discard-only
	{"100:0:0:1::/64", []string{"100:0:0:1::1"}},       // dummy prefix, RFC 9780
	{"2001::/32", []string{"2001::1"}},                 // Teredo
	{"2001:2::/48", []string{"2001:2::1"}},             // benchmarking
	{"2001:db8::/32", []string{"2001:db8::1"}},         // documentation
	{"3fff::/20", []string{"3fff::1", "3fff:fff::1"}},  // documentation
	{"5f00::/16", []string{"5f00::1", "5f00:ffff::1"}}, // SRv6 SIDs
	{"fc00::/7", []string{"fc00::1", "fd12:3456::1"}},  // unique local
	{"fe80::/10", []string{"fe80::1", "febf:ffff::1"}}, // link-local
}

// n2Allowed: addresses that MUST stay allowed — real public IPs plus every
// sample IANA explicitly marks Globally Reachable = True that a naive
// special-use blocklist might wrongly refuse. (The previous version of this
// oracle wrongly demanded refusal of the AS112/AMT/PCP/TURN ranges below;
// the registries mark all of them globally reachable.)
var n2Allowed = []string{
	"1.1.1.1", "8.8.8.8", "9.9.9.9", "93.184.215.14",
	"2606:4700:4700::1111", "2001:4860:4860::8888",
	"192.31.196.1",     // AS112-v4, IANA Globally Reachable True
	"192.52.193.1",     // AMT, IANA Globally Reachable True
	"192.175.48.1",     // Direct Delegation AS112, IANA Globally Reachable True
	"2001:1::1",        // PCP anycast, IANA Globally Reachable True
	"2001:1::2",        // TURN anycast, IANA Globally Reachable True
	"2001:3::1",        // AMT, IANA Globally Reachable True
	"2001:4:112::1",    // AS112-v6, IANA Globally Reachable True
	"2620:4f:8000::1",  // Direct Delegation AS112 Service, IANA Globally Reachable True
	"::ffff:0:1.1.1.1", // SIIT spelling of a public IPv4 address
}

// TestN2_OracleRefusesNotGloballyReachable asserts the vendored guard refuses
// a sample address in every prefix IANA marks not globally reachable.
func TestN2_OracleRefusesNotGloballyReachable(t *testing.T) {
	for _, tc := range n2Refused {
		p := netip.MustParsePrefix(tc.prefix)
		for _, s := range tc.addrs {
			a := netip.MustParseAddr(s)
			if !p.Contains(a) {
				t.Fatalf("oracle bug: %s is not in %s", s, tc.prefix)
			}
			if netguard.Public(a) {
				t.Errorf("%s (%s, IANA: not globally reachable) was ALLOWED but must be refused",
					s, tc.prefix)
			}
		}
	}
	t.Logf("oracle: %d not-globally-reachable prefixes all refused", len(n2Refused))
}

// TestN2_OracleAllowsGloballyReachable asserts the vendored guard allows
// real public IPs and every address IANA marks globally reachable.
func TestN2_OracleAllowsGloballyReachable(t *testing.T) {
	for _, s := range n2Allowed {
		if !netguard.Public(netip.MustParseAddr(s)) {
			t.Errorf("%s was REFUSED but must be allowed", s)
		}
	}
	t.Logf("oracle: %d globally-reachable addresses all allowed", len(n2Allowed))
}

// TestN2_OracleSIITSpellings checks IPv4-mapped spellings: ones embedding
// non-public IPv4 must be refused, ones embedding public IPv4 allowed.
func TestN2_OracleSIITSpellings(t *testing.T) {
	refused := []struct{ spelling, v4 string }{
		{"::ffff:10.0.0.1", "10.0.0.1"},
		{"::ffff:0a00:0001", "10.0.0.1"}, // hex spelling
		{"::ffff:192.168.1.1", "192.168.1.1"},
		{"::ffff:c0a8:0101", "192.168.1.1"}, // hex spelling
		{"::ffff:127.0.0.1", "127.0.0.1"},
		{"::ffff:7f00:0001", "127.0.0.1"}, // hex spelling
		{"::ffff:169.254.10.20", "169.254.10.20"},
	}
	for _, tc := range refused {
		a := netip.MustParseAddr(tc.spelling)
		if a.Unmap().String() != tc.v4 {
			t.Logf("note: %s unmaps to %s (expected %s)", tc.spelling, a.Unmap(), tc.v4)
		}
		if netguard.Public(a) {
			t.Errorf("SIIT form %s (embeds %s) was ALLOWED but must be refused", tc.spelling, tc.v4)
		}
	}
	allowed := []string{"::ffff:0:1.1.1.1", "::ffff:0808:0808"} // embed 1.1.1.1 / 8.8.8.8
	for _, s := range allowed {
		if !netguard.Public(netip.MustParseAddr(s)) {
			t.Errorf("SIIT form %s (embeds public IPv4) was REFUSED but must be allowed", s)
		}
	}
	t.Logf("oracle: %d SIIT spellings classified correctly", len(refused)+len(allowed))
}
