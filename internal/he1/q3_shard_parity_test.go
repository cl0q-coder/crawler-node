package he1

// Q3 (#283): shard/fold parity as a permanent test.
//
// The shard mapping is coordination-critical: the central lease service and
// every node must agree, forever, on which shard a domain belongs to. These
// tests lock down the two layers independently:
//
//  1. TestQ3ShardGolden — a fixed corpus pinned to the current ShardOf
//     values at 1024 shards. Any change to the fold, the hash, or the
//     modulo breaks loudly; drift must be a deliberate, reviewed change.
//  2. TestQ3FoldProperties — the registrable fold's invariants:
//     idempotence, subdomain co-sharding (per-host politeness depends on
//     it), and input normalization.
//  3. TestQ3FormulaParity — an independent re-implementation of the
//     documented formula (hand-rolled FNV-1a from the spec constants, no
//     hash/fnv import) over keys derived per the documented rule. Fails if
//     ShardOf itself drifts from its own spec.
//
// Tests-only; no production code is touched by this file.

import (
	"math/rand"
	"strings"
	"testing"

	"cl0q/crawlutil"
)

// q3GoldenDomains is the fixed corpus for the golden test. The expected
// shard values live in q3GoldenShards, generated from the implementation
// (see zz_emit_test.go, deleted after generation) and eyeball-verified for
// the grouping invariants below.
var q3GoldenDomains = []string{
	// Plain registrable + normalization variants (all must agree).
	"example.com",
	"www.example.com",
	"EXAMPLE.COM",
	"example.com.",
	"  example.com  ",
	// Subdomain grouping (per-host politeness depends on this).
	"blog.example.com",
	"deep.blog.example.com",
	"a.b.c.d.example.com",
	// Multi-label public suffix.
	"bbc.co.uk",
	"www.bbc.co.uk",
	"sport.bbc.co.uk",
	// Private suffixes are skipped on purpose (subdomains stay separate).
	"foo.blogspot.com",
	"bar.foo.blogspot.com",
	"user.github.io",
	// African multi-label suffix.
	"nation.co.ke",
	"sport.nation.co.ke",
	// IP literals hash on the normalized host itself.
	"192.0.2.1",
	"10.0.0.1",
	"::1",
	"2001:db8::1",
	// Public suffixes / degenerate inputs fold to "" -> normalized host.
	"com",
	"co.ke",
	"co.uk",
	"",
	"   ",
	".",
	"a",
	"a..com",
	// Unicode / IDN input (pinned whatever the fold yields).
	"münchen.de",
	"xn--mnchen-3ya.de",
	// Long tail.
	"shop.example-store.io",
	"api.v2.service.example.net",
}

// q3GoldenShards maps each q3GoldenDomains entry to its ShardOf(domain, 1024).
// Filled by the emitter; eyeball-verified against the invariants in
// TestQ3GoldenInvariants before pinning.
var q3GoldenShards = []uint16{
	198, 198, 198, 198, 198, 198, 198, 198, // example.com group
	812, 812, 812, // bbc.co.uk group
	558, 558, 88, // blogspot group, user.github.io
	516, 516, // nation.co.ke group
	536, 909, 940, 743, // IP literals
	478, 31, 227, // bare public suffixes
	805, 805, 945, 140, 901, // degenerate inputs
	564, 313, // IDN: note unicode vs punycode forms diverge (fold does no IDNA normalization)
	1013, 944,
}

func TestQ3ShardGolden(t *testing.T) {
	if len(q3GoldenShards) != len(q3GoldenDomains) {
		t.Fatalf("golden table mismatch: %d domains vs %d shards", len(q3GoldenDomains), len(q3GoldenShards))
	}
	for i, d := range q3GoldenDomains {
		if got := ShardOf(d, 1024); got != q3GoldenShards[i] {
			t.Errorf("ShardOf(%q, 1024) = %d, golden = %d — the shard mapping drifted; this must be a deliberate, reviewed change", d, got, q3GoldenShards[i])
		}
	}
}

// TestQ3GoldenInvariants re-states the grouping invariants the corpus must
// satisfy, so a bad emitter run can't silently pin nonsense.
func TestQ3GoldenInvariants(t *testing.T) {
	// Everything that folds to example.com shares one shard.
	for _, d := range []string{"example.com", "www.example.com", "EXAMPLE.COM", "example.com.", "  example.com  ", "blog.example.com", "deep.blog.example.com", "a.b.c.d.example.com"} {
		if ShardOf(d, 1024) != ShardOf("example.com", 1024) {
			t.Errorf("%q must co-shard with example.com", d)
		}
	}
	// bbc.co.uk group; blogspot subdomains stay on blogspot.com, not on the
	// private suffix.
	if ShardOf("sport.bbc.co.uk", 1024) != ShardOf("bbc.co.uk", 1024) {
		t.Error("sport.bbc.co.uk must co-shard with bbc.co.uk")
	}
	if ShardOf("bar.foo.blogspot.com", 1024) != ShardOf("foo.blogspot.com", 1024) {
		t.Error("bar.foo.blogspot.com must co-shard with foo.blogspot.com")
	}
	if ShardOf("foo.blogspot.com", 1024) != ShardOf("blogspot.com", 1024) {
		t.Error("foo.blogspot.com must fold to blogspot.com")
	}
	// IP literals are stable and distinct-ish.
	if ShardOf("192.0.2.1", 1024) == ShardOf("10.0.0.1", 1024) {
		t.Error("distinct IP literals collided (possible but suspicious in a 2-sample check)")
	}
}

// TestQ3FoldIdempotent: the fold must be a fixed point.
func TestQ3FoldIdempotent(t *testing.T) {
	inputs := append([]string{}, q3GoldenDomains...)
	r := rand.New(rand.NewSource(283))
	for i := 0; i < 500; i++ {
		inputs = append(inputs, q3RandHost(r))
	}
	for _, in := range inputs {
		once := crawlutil.Registrable(in)
		twice := crawlutil.Registrable(once)
		if once != twice {
			t.Errorf("Registrable not idempotent: %q -> %q -> %q", in, once, twice)
		}
	}
}

// TestQ3CoShardProperty: any host whose registrable fold is r must shard
// with r. Fuzz-ish over generated multi-label hosts (fixed seed).
func TestQ3CoShardProperty(t *testing.T) {
	r := rand.New(rand.NewSource(135))
	for i := 0; i < 2000; i++ {
		base := q3RandBase(r)
		sub := q3RandSubdomain(r, base)
		// Only assert when the fold actually maps sub -> base; when the
		// fold says otherwise (private suffixes, IPs) there is nothing
		// to assert about grouping.
		if got := crawlutil.Registrable(sub); got == base {
			if ShardOf(sub, 1024) != ShardOf(base, 1024) {
				t.Fatalf("co-sharding violated: sub=%q base=%q fold=%q", sub, base, got)
			}
		}
	}
}

// TestQ3Normalization: case, trailing dots, and surrounding whitespace must
// not move a host between shards.
func TestQ3Normalization(t *testing.T) {
	pairs := [][2]string{
		{"Example.COM", "example.com"},
		{"EXAMPLE.com.", "example.com"},
		{"  Blog.Example.COM  ", "blog.example.com"},
		{"WWW.BBC.CO.UK", "bbc.co.uk"},
		{"192.0.2.1 ", "192.0.2.1"},
	}
	for _, p := range pairs {
		if ShardOf(p[0], 1024) != ShardOf(p[1], 1024) {
			t.Errorf("normalization split shards: %q vs %q", p[0], p[1])
		}
	}
}

// TestQ3ShardRange: ids stay inside [0, shardCount) for several space sizes,
// and non-positive counts fall back to the default space.
func TestQ3ShardRange(t *testing.T) {
	for _, k := range []int{16, 1024, 65535, 0, -5} {
		want := k
		if want <= 0 {
			want = DefaultShardCount
		}
		for _, d := range q3GoldenDomains {
			if s := ShardOf(d, k); int(s) >= want {
				t.Errorf("ShardOf(%q, %d) = %d, out of range", d, k, s)
			}
		}
	}
}

// q3FNV1a64 is an independent FNV-1a/64 implementation straight from the
// spec constants — deliberately not hash/fnv, so TestQ3FormulaParity fails
// if ShardOf's hash primitive ever changes.
func q3FNV1a64(s string) uint64 {
	const (
		offsetBasis = 14695981039346656037
		prime       = 1099511628211
	)
	h := uint64(offsetBasis)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// q3SpecShard re-implements the documented ShardOf formula independently:
// key = registrable fold (fallback: lowercased trimmed host), then
// FNV-1a(key) mod shardCount.
func q3SpecShard(domain string, shardCount int) uint16 {
	if shardCount <= 0 {
		shardCount = DefaultShardCount
	}
	key := crawlutil.Registrable(domain)
	if key == "" {
		key = strings.ToLower(strings.TrimSpace(domain))
	}
	return uint16(q3FNV1a64(key) % uint64(shardCount))
}

// TestQ3FormulaParity: ShardOf must equal the independent spec
// re-implementation on the corpus and on generated hosts, at several
// shard-space sizes.
func TestQ3FormulaParity(t *testing.T) {
	hosts := append([]string{}, q3GoldenDomains...)
	r := rand.New(rand.NewSource(284))
	for i := 0; i < 1000; i++ {
		hosts = append(hosts, q3RandHost(r))
	}
	for _, k := range []int{16, 1024, 4096} {
		for _, h := range hosts {
			if got, want := ShardOf(h, k), q3SpecShard(h, k); got != want {
				t.Fatalf("formula drift: ShardOf(%q, %d) = %d, spec = %d", h, k, got, want)
			}
		}
	}
}

var q3Labels = []string{"blog", "shop", "api", "cdn", "mail", "dev", "a1", "x", "deep", "www2"}

var q3Bases = []string{
	"example.com", "bbc.co.uk", "nation.co.ke", "foo.blogspot.com",
	"shop.example-store.io", "service.example.net", "münchen.de",
}

func q3RandBase(r *rand.Rand) string {
	return q3Bases[r.Intn(len(q3Bases))]
}

// q3RandHost builds a plausible crawl-path host: optional deep subdomains,
// occasional case/whitespace/trailing-dot noise, occasional IP literal.
func q3RandHost(r *rand.Rand) string {
	if r.Intn(10) == 0 {
		ips := []string{"192.0.2.1", "10.1.2.3", "2001:db8::7"}
		return ips[r.Intn(len(ips))]
	}
	depth := r.Intn(4)
	parts := make([]string, 0, depth+1)
	for i := 0; i < depth; i++ {
		parts = append(parts, q3Labels[r.Intn(len(q3Labels))])
	}
	host := strings.Join(append(parts, q3RandBase(r)), ".")
	switch r.Intn(6) {
	case 0:
		host = strings.ToUpper(host)
	case 1:
		host += "."
	case 2:
		host = " " + host + " "
	}
	return host
}

func q3RandSubdomain(r *rand.Rand, base string) string {
	depth := 1 + r.Intn(3)
	parts := make([]string, 0, depth+1)
	for i := 0; i < depth; i++ {
		parts = append(parts, q3Labels[r.Intn(len(q3Labels))])
	}
	return strings.Join(append(parts, base), ".")
}
