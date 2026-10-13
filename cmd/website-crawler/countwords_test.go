package main

// Tests for the #274 phase-2 countWords byte fast path: the table-driven
// ASCII path must agree with the full unicode.IsSpace rune loop on all inputs.

import (
	"strings"
	"testing"
	"unicode"
)

// countWordsRef is the pre-#274 implementation: decode every rune, full
// unicode.IsSpace lookup. The fast path must match it exactly.
func countWordsRef(s string) int {
	n, in := 0, false
	for _, r := range s {
		if unicode.IsSpace(r) {
			in = false
		} else if !in {
			in, n = true, n+1
		}
	}
	return n
}

func TestCountWordsEquivalence(t *testing.T) {
	lorem := "Lorem ipsum dolor sit amet, consectetur adipiscing elit. "
	cases := map[string]string{
		"empty":       "",
		"spaces":      "   \t\n\r\f\v   ",
		"simple":      "hello world",
		"lorem":       strings.Repeat(lorem, 50),
		"japanese":    "こんにちは 世界 テスト",
		"cjk-mixed":   "hello世界 foo barバズ",
		"nbsp":        "a\u00a0b\u00a0\u00a0c", // NBSP is unicode space
		"em-space":    "a\u2003b",              // EM SPACE
		"ideographic": "a\u3000b",              // ideographic space
		"zero-width":  "a\u200bb",              // zero-width space is NOT space
		"trailing":    "word   ",
		"leading":     "   word",
		"newlines":    "a\n\nb\rc\rd",
		"mixed-ws":    "a \u00a0 \u2003 \t b",
		"emoji":       "😀 smile 😀 grin",
		"combining":   "e\u0301clair resume\u0301",
		"long-ascii":  strings.Repeat(lorem, 2000),
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			if got, want := countWords(s), countWordsRef(s); got != want {
				t.Errorf("countWords(%q) = %d, ref = %d", s, got, want)
			}
		})
	}
}

func benchWordsText() string {
	lorem := "Lorem ipsum dolor sit amet, consectetur adipiscing elit. Sed do eiusmod tempor incididunt. "
	return strings.Repeat(lorem, 2000) // ~180KB
}

func BenchmarkCountWordsOld(b *testing.B) {
	s := benchWordsText()
	want := countWordsRef(s)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if countWordsRef(s) != want {
			b.Fatal("mismatch")
		}
	}
}

func BenchmarkCountWordsFast(b *testing.B) {
	s := benchWordsText()
	want := countWordsRef(s)
	if countWords(s) != want {
		b.Fatalf("fast path disagrees: %d vs %d", countWords(s), want)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		countWords(s)
	}
}
