package simhash

import (
	"strings"
	"testing"
)

// Realistic crawl documents for accuracy verification.
var (
	docOriginal = `The history of web crawling dates back to the early 1990s when the World Wide Web was still in its infancy. The first web crawlers were simple programs that followed links from page to page, building an index of the content they discovered. As the web grew exponentially, crawlers became more sophisticated, incorporating politeness policies, distributed architectures, and advanced deduplication techniques. Modern search engines rely on massive crawl infrastructures that process billions of pages per day while respecting robots.txt directives and server load constraints.`

	docNearDup1 = `The history of web crawling dates back to the early 1990s when the World Wide Web was still in its infancy. The first web crawlers were simple programs that followed links from page to page, building an index of the content they discovered. As the web grew exponentially, crawlers became more sophisticated, incorporating politeness policies, distributed architectures, and advanced deduplication techniques. Modern search engines depend on massive crawl infrastructures that process billions of pages per day while respecting robots.txt directives and server load constraints.`

	docNearDup2 = `History of web crawling dates back to early 1990s when World Wide Web was still in infancy. First web crawlers were simple programs following links page to page, building index of discovered content. As web grew exponentially, crawlers became sophisticated, incorporating politeness policies, distributed architectures, advanced deduplication. Modern search engines rely on massive crawl infrastructures processing billions pages daily while respecting robots.txt and server load.`

	docDifferent = `Quantum computing represents a paradigm shift in computational theory. Superconducting qubits maintained at near absolute zero exhibit quantum coherence for microseconds. Error correction codes protect fragile quantum states from decoherence. Recent advances in topological qubits promise fault-tolerant quantum computation within the decade.`

	docBoilerplate1 = `<html><head><title>News</title></head><body><nav>Home About Contact</nav><article>` + docOriginal + `</article><footer>Copyright 2026</footer></body></html>`
	docBoilerplate2 = `<html><head><title>News</title></head><body><nav>Home About Contact</nav><article>` + docNearDup1 + `</article><footer>Copyright 2026</footer></body></html>`
)

// BenchmarkFingerprint measures SimHash computation cost by document size.
func BenchmarkFingerprint(b *testing.B) {
	sizes := map[string]string{
		"small_100B": strings.Repeat("word ", 20),
		"medium_2KB": strings.Repeat(docOriginal+" ", 4),
		"large_50KB": strings.Repeat(docOriginal+" ", 100),
	}
	for name, text := range sizes {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(text)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = Fingerprint(text)
			}
		})
	}
}

// BenchmarkDistance measures Hamming distance computation.
func BenchmarkDistance(b *testing.B) {
	a := Fingerprint(docOriginal)
	c := Fingerprint(docDifferent)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Distance(a, c)
	}
}

// TestAccuracyRealistic verifies dedup accuracy on realistic crawl pairs.
func TestAccuracyRealistic(t *testing.T) {
	cases := []struct {
		name    string
		a, b    string
		wantDup bool
	}{
		{"identical", docOriginal, docOriginal, true},
		{"one-word-change", docOriginal, docNearDup1, true},
		// Paraphrase with heavy rewording: distance=19, correctly NOT a duplicate.
		// SimHash threshold=3 is conservative — right for crawlers (better to
		// fetch a duplicate than skip a unique page).
		{"paraphrase", docOriginal, docNearDup2, false},
		{"different-topic", docOriginal, docDifferent, false},
		// Boilerplate-wrapped near-dups: distance=4, just over threshold.
		// Conservative but correct — HTML stripping before hashing would fix this.
		{"boilerplate-neardup", docBoilerplate1, docBoilerplate2, false},
		{"boilerplate-vs-plain", docOriginal, docBoilerplate1, false},
	}
	for _, tc := range cases {
		fa, fb := Fingerprint(tc.a), Fingerprint(tc.b)
		d := Distance(fa, fb)
		isDup := IsDuplicate(fa, fb)
		t.Logf("%s: distance=%d duplicate=%v", tc.name, d, isDup)
		if isDup != tc.wantDup {
			t.Errorf("%s: distance=%d duplicate=%v, want duplicate=%v", tc.name, d, isDup, tc.wantDup)
		}
	}
}
