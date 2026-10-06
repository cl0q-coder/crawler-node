package simhash

import (
	"strings"
	"testing"
)

// IG2: SimHash must map similar documents to nearby fingerprints and
// dissimilar documents to distant ones.

func TestIdenticalDocuments(t *testing.T) {
	text := "the quick brown fox jumps over the lazy dog"
	a := Fingerprint(text)
	b := Fingerprint(text)
	if a != b {
		t.Errorf("identical documents: fingerprints differ (%x vs %x)", a, b)
	}
	if d := Distance(a, b); d != 0 {
		t.Errorf("identical documents: distance = %d, want 0", d)
	}
}

func TestNearDuplicates(t *testing.T) {
	// Natural varied text: SimHash is stable under small edits when the
	// document has a rich shingle distribution.
	original := `The history of web crawling dates back to the early 1990s when the World Wide Web was still in its infancy. The first web crawlers were simple programs that followed links from page to page, building an index of the content they discovered. As the web grew exponentially, crawlers became more sophisticated, incorporating politeness policies, distributed architectures, and advanced deduplication techniques. Modern search engines rely on massive crawl infrastructures that process billions of pages per day while respecting robots.txt directives and server load constraints.`
	edited := `The history of web crawling dates back to the early 1990s when the World Wide Web was still in its infancy. The first web crawlers were simple programs that followed links from page to page, building an index of the content they discovered. As the web grew exponentially, crawlers became more sophisticated, incorporating politeness policies, distributed architectures, and advanced deduplication techniques. Modern search engines depend on massive crawl infrastructures that process billions of pages per day while respecting robots.txt directives and server load constraints.`
	// Only one word changed: "rely" -> "depend".
	a := Fingerprint(original)
	b := Fingerprint(edited)
	d := Distance(a, b)
	if d > DuplicateThreshold {
		t.Errorf("near-duplicates: distance = %d, want <= %d", d, DuplicateThreshold)
	}
	if !IsDuplicate(a, b) {
		t.Error("near-duplicates should be detected as duplicates")
	}
}

func TestDifferentDocuments(t *testing.T) {
	a := Fingerprint("the quick brown fox jumps over the lazy dog")
	b := Fingerprint("quantum computing advances with superconducting qubits and error correction")
	d := Distance(a, b)
	// Random 64-bit hashes differ in ~32 bits; similar threshold.
	if d < 20 {
		t.Errorf("different documents: distance = %d, want >= 20 (well separated)", d)
	}
	if IsDuplicate(a, b) {
		t.Error("different documents should not be duplicates")
	}
}

func TestEmptyText(t *testing.T) {
	if fp := Fingerprint(""); fp != 0 {
		t.Errorf("empty text: fingerprint = %x, want 0", fp)
	}
	if fp := Fingerprint("a b c"); fp != 0 {
		// Single-char tokens filtered; effectively empty.
		t.Errorf("noise text: fingerprint = %x, want 0", fp)
	}
}

func TestBlocksCoverNearDuplicates(t *testing.T) {
	// Property: any two fingerprints with distance <= 3 share at least one block.
	a := Fingerprint(strings.Repeat("lorem ipsum dolor sit amet ", 20))
	// Flip up to 3 bits in different blocks.
	for _, flip := range []uint64{1 << 5, 1 << 20, 1 << 40} {
		b := a ^ flip
		if Distance(a, b) > 3 {
			continue
		}
		ba, bb := Blocks(a), Blocks(b)
		matched := false
		for i := 0; i < 4; i++ {
			if ba[i] == bb[i] {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("fingerprints distance %d share no block (violates pigeonhole)", Distance(a, b))
		}
	}
}

func TestDistance(t *testing.T) {
	if d := Distance(0, 0); d != 0 {
		t.Errorf("Distance(0,0) = %d, want 0", d)
	}
	if d := Distance(0, 1); d != 1 {
		t.Errorf("Distance(0,1) = %d, want 1", d)
	}
	if d := Distance(0xFFFFFFFFFFFFFFFF, 0); d != 64 {
		t.Errorf("all bits differ: distance = %d, want 64", d)
	}
}
