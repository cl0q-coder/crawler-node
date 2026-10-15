// K3 (near-duplicate clustering): per-page 64-bit SimHash fingerprints
// (HE-4's SimHash-delta infra — no new fingerprinting here) clustered locally
// by Hamming banding + union-find. Same additive pattern as K1/K2: a pure
// detector, no crawl-behavior change. The pre-index gate wire-in is a
// separate PR and holds for Claude's final say (Track K, crawler-node #284).
//
// Design (posted on #284, Micah's knob calls 2026-10-14):
//   - Local tier only: cluster the fingerprint set handed to the function
//     (a flush batch, or a candidate set from the simhash_b* columns). The
//     fleet tier (coordinator-side merge via the HE-1 lease wire, PROTOCOL
//     v2) is Claude's lane — this file does not reach for it.
//   - Banding: split the 64-bit fingerprint into (threshold+1) bands;
//     pigeonhole guarantees any pair within Hamming distance <= threshold
//     shares at least one band exactly. Candidate pairs (shared band) are
//     confirmed by exact Hamming distance, then merged by union-find — so
//     transitive chains (A~B, B~C, but A!/~~C) form one cluster.
//   - NO suppression in the detector (Micah's call): clusters of size 2+ are
//     reported honestly, including 2-member clusters — a pair can be a legit
//     mirror or the seed of a farm, and the wire-in needs to see both. The
//     wire-in PR decides suppression policy.
//   - Representative = oldest crawl (first-seen is the best proxy for "the
//     original"). An authority score (max in cluster) rides in the verdict
//     so the wire-in can pick highest-authority instead if index quality
//     ever wants it.
//   - Disambiguation signals live IN the verdict, not in a binary call:
//     distinct registrable-domain count + sample domains. Many distinct
//     domains with identical content = farm smell; 2-3 domains = legit
//     mirror. ParkedMembers cross-refs the K1 parked verdict — parked
//     domains love forming farms.
//
// FRESHNESS CAVEAT (Claude's RED finding, crawler-node #283, fix is #297):
// the batch upsert (flushBatchTx -> upsertTail) never refreshes SimHash on
// conflict, so fingerprints fed from website_info can be stale — K3 clusters
// are only as fresh as the stored fingerprints. The #297 fix will refresh
// them on upsert.
package quality

import (
	"sort"
	"time"
)

// NearDupHammingThreshold is the default max Hamming distance for two pages
// to count as near-duplicates. Micah's call (2026-10-14): start at 3 —
// conservative is right for a first cut (SafeSearch over-filtering lesson);
// measured precision/recall on the labeled sample argues for 5 later if the
// data supports it. The threshold is a parameter, not a constant: raising it
// re-bands the fingerprints (bands = threshold+1) so recall stays exact.
const NearDupHammingThreshold = 3

// maxSampleDomains caps ClusterVerdict.SampleDomains so the verdict stays
// small. DistinctDomainCount always carries the full count.
const maxSampleDomains = 5

// NearDupPage is the per-page input to the K3 clusterer: identity and
// fingerprint fields. The future wire-in builds these from the crawl batch
// (result.domain, info.SimHash, web_crawled_at for FirstSeen, and
// opic_history + opic_cash for Authority) plus the K1 DetectParked verdict
// the gate already computed per page.
type NearDupPage struct {
	URL       string    // canonical page URL; used as the representative URL
	Domain    string    // registrable domain, e.g. "example.com"
	SimHash   uint64    // HE-4/IG2 64-bit fingerprint; 0 = not computed → skipped
	FirstSeen time.Time // first time this page was crawled; oldest becomes representative
	Authority float64   // link-earned importance (opic_history + opic_cash); 0 = unknown
	Parked    bool      // K1 DetectParked verdict for this page — cross-ref signal
}

// ClusterVerdict is the K3 near-dup verdict for one input page. Verdicts are
// returned in input order, one per page.
type ClusterVerdict struct {
	IsNearDup           bool     // member of a near-dup cluster of size >= 2
	ClusterSize         int      // pages in this page's cluster (1 = singleton, not a dup)
	RepresentativeURL   string   // URL of the oldest-crawled (first-seen) member
	Confidence          float64  // 1 - distToRepresentative/(threshold+1), in (0,1] for members; 0 for singletons
	DistinctDomainCount int      // distinct registrable domains in the cluster — farm smell when high
	SampleDomains       []string // up to maxSampleDomains sorted domains, for the disambiguation heuristic
	AuthorityScore      float64  // max Authority in the cluster; wire-in can pick highest-authority instead
	ParkedMembers       int      // cluster members whose K1 verdict was parked — parked domains love farms
}

// unionFind is a small disjoint-set for the band-confirmed candidate pairs.
type unionFind struct{ parent []int }

func newUnionFind(n int) *unionFind {
	p := make([]int, n)
	for i := range p {
		p[i] = i
	}
	return &unionFind{parent: p}
}

func (u *unionFind) find(x int) int {
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]]
		x = u.parent[x]
	}
	return x
}

func (u *unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra != rb {
		u.parent[rb] = ra
	}
}

// bands splits fp into (threshold+1) contiguous bands. Any two fingerprints
// within Hamming distance <= threshold share at least one band exactly
// (pigeonhole: threshold differing bits can touch at most threshold bands),
// so banding is recall-exact for every threshold — including 5, if the
// labeled sample ever argues for raising it.
func bands(fp uint64, threshold int) []uint64 {
	nb := threshold + 1
	out := make([]uint64, nb)
	for i := 0; i < nb; i++ {
		lo := i * 64 / nb
		hi := (i + 1) * 64 / nb
		width := hi - lo
		var mask uint64
		if width >= 64 {
			mask = ^uint64(0)
		} else {
			mask = (uint64(1) << uint(width)) - 1
		}
		out[i] = (fp >> uint(lo)) & mask
	}
	return out
}

// hammingDistance counts differing bits between two fingerprints.
func hammingDistance(a, b uint64) int {
	x := a ^ b
	n := 0
	for x != 0 {
		n++
		x &= x - 1
	}
	return n
}

// ClusterNearDup clusters pages by SimHash at the default Hamming threshold.
func ClusterNearDup(pages []NearDupPage) []ClusterVerdict {
	return ClusterNearDupWithThreshold(pages, NearDupHammingThreshold)
}

// ClusterNearDupWithThreshold clusters pages by SimHash at the given Hamming
// threshold. Pure: no I/O, no DB. Pages with SimHash == 0 (fingerprint not
// computed) are skipped and come back as singletons. Output is one
// ClusterVerdict per input page, in input order.
func ClusterNearDupWithThreshold(pages []NearDupPage, threshold int) []ClusterVerdict {
	if threshold < 0 {
		threshold = 0
	}
	n := len(pages)
	verdicts := make([]ClusterVerdict, n)
	for i, p := range pages {
		verdicts[i] = ClusterVerdict{
			IsNearDup:           false,
			ClusterSize:         1,
			RepresentativeURL:   p.URL,
			Confidence:          0,
			DistinctDomainCount: 1,
			SampleDomains:       []string{p.Domain},
			AuthorityScore:      p.Authority,
			ParkedMembers:       boolToInt(p.Parked),
		}
	}
	if n == 0 {
		return verdicts
	}

	// Eligible members: fingerprint computed.
	members := make([]int, 0, n)
	for i, p := range pages {
		if p.SimHash != 0 {
			members = append(members, i)
		}
	}
	if len(members) < 2 {
		return verdicts
	}

	// Band the eligible fingerprints; bucket page indices by (band, value).
	type bandKey struct {
		band int
		val  uint64
	}
	buckets := map[bandKey][]int{}
	fps := map[int]uint64{}
	for _, i := range members {
		fp := pages[i].SimHash
		fps[i] = fp
		for b, v := range bands(fp, threshold) {
			k := bandKey{b, v}
			buckets[k] = append(buckets[k], i)
		}
	}

	// Union band-sharing pairs whose exact Hamming distance is in threshold.
	uf := newUnionFind(n)
	for _, bucket := range buckets {
		for a := 0; a < len(bucket); a++ {
			for b := a + 1; b < len(bucket); b++ {
				i, j := bucket[a], bucket[b]
				if uf.find(i) == uf.find(j) {
					continue
				}
				if hammingDistance(fps[i], fps[j]) <= threshold {
					uf.union(i, j)
				}
			}
		}
	}

	// Group members by root; deterministic order (input index order).
	groups := map[int][]int{}
	for _, i := range members {
		r := uf.find(i)
		groups[r] = append(groups[r], i)
	}

	// Emit cluster verdicts for groups of size >= 2.
	for _, g := range groups {
		if len(g) < 2 {
			continue
		}
		// Representative: oldest FirstSeen; tie-break by smallest URL so
		// the choice is stable across batch orderings.
		rep := g[0]
		for _, i := range g[1:] {
			pi, pr := pages[i], pages[rep]
			if pi.FirstSeen.Before(pr.FirstSeen) ||
				(pi.FirstSeen.Equal(pr.FirstSeen) && pi.URL < pr.URL) {
				rep = i
			}
		}
		repFP := fps[rep]

		domains := map[string]bool{}
		maxAuth := 0.0
		parked := 0
		for _, i := range g {
			domains[pages[i].Domain] = true
			if pages[i].Authority > maxAuth {
				maxAuth = pages[i].Authority
			}
			parked += boolToInt(pages[i].Parked)
		}
		sorted := make([]string, 0, len(domains))
		for d := range domains {
			sorted = append(sorted, d)
		}
		sort.Strings(sorted)
		sample := sorted
		if len(sample) > maxSampleDomains {
			sample = sample[:maxSampleDomains]
		}

		for _, i := range g {
			d := hammingDistance(fps[i], repFP)
			verdicts[i] = ClusterVerdict{
				IsNearDup:           true,
				ClusterSize:         len(g),
				RepresentativeURL:   pages[rep].URL,
				Confidence:          1 - float64(d)/float64(threshold+1),
				DistinctDomainCount: len(domains),
				SampleDomains:       append([]string(nil), sample...),
				AuthorityScore:      maxAuth,
				ParkedMembers:       parked,
			}
		}
	}
	return verdicts
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
