// Package simhash computes 64-bit SimHash fingerprints for near-duplicate
// detection (IG2). ~30% of web pages are duplicates or near-duplicates;
// crawling and indexing them wastes budget.
//
// SimHash is locality-sensitive: similar documents get similar hashes.
// Two fingerprints with Hamming distance <= 3 are near-duplicates.
//
// The algorithm: tokenize into word shingles, hash each shingle to 64 bits,
// then for each bit position, sum +1 (bit set) or -1 (bit clear) weighted by
// shingle frequency. Bits with positive sum become 1 in the fingerprint.
package simhash

import (
	"hash/fnv"
	"strings"
	"unicode"
)

// Fingerprint computes the 64-bit SimHash of text.
func Fingerprint(text string) uint64 {
	// Tokenize into words (lowercased, alphanumeric).
	words := tokenize(text)
	if len(words) == 0 {
		return 0
	}

	// Count shingle frequencies (unigrams + bigrams for better discrimination).
	freq := map[uint64]int{}
	for i, w := range words {
		h := hashString(w)
		freq[h]++
		if i+1 < len(words) {
			bigram := w + " " + words[i+1]
			hb := hashString(bigram)
			freq[hb]++
		}
	}

	// Sum bit vectors.
	var sums [64]int
	for h, count := range freq {
		for i := 0; i < 64; i++ {
			if h&(1<<uint(i)) != 0 {
				sums[i] += count
			} else {
				sums[i] -= count
			}
		}
	}

	// Build fingerprint from positive sums.
	var fp uint64
	for i := 0; i < 64; i++ {
		if sums[i] > 0 {
			fp |= 1 << uint(i)
		}
	}
	return fp
}

// Distance returns the Hamming distance between two fingerprints
// (number of differing bits).
func Distance(a, b uint64) int {
	x := a ^ b
	n := 0
	for x != 0 {
		n++
		x &= x - 1 // clear lowest set bit
	}
	return n
}

// DuplicateThreshold is the max Hamming distance for near-duplicates.
// 3 bits out of 64 is the standard threshold (Charikar's original work).
const DuplicateThreshold = 3

// IsDuplicate reports whether two fingerprints are near-duplicates.
func IsDuplicate(a, b uint64) bool {
	return Distance(a, b) <= DuplicateThreshold
}

// Blocks splits a fingerprint into 4 blocks of 16 bits for indexed lookup.
// Near-duplicates (distance <= 3) are guaranteed to match in at least one
// block (pigeonhole principle: 3 differing bits can affect at most 3 blocks).
func Blocks(fp uint64) [4]uint16 {
	return [4]uint16{
		uint16(fp >> 48),
		uint16(fp >> 32),
		uint16(fp >> 16),
		uint16(fp),
	}
}

func tokenize(text string) []string {
	// Split on non-alphanumeric, lowercase.
	f := func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}
	words := strings.FieldsFunc(strings.ToLower(text), f)
	// Filter very short tokens (noise).
	out := words[:0]
	for _, w := range words {
		if len(w) >= 2 {
			out = append(out, w)
		}
	}
	return out
}

func hashString(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}
