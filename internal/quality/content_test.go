package quality

import (
	"strings"
	"testing"
)

// A healthy, real article scores high and carries positive features.
func TestScoreContentHealthy(t *testing.T) {
	body := "The web archive project preserves pages. " + strings.Repeat("Reporters documented the rollout and volunteers reviewed each entry. ", 40)
	cp := ContentPage{
		Page:         Page{Domain: "example.com", Title: "How the archive works", Text: body, WordCount: 380},
		HeadingCount: 3,
		NearDupSize:  1,
	}
	s := ScoreContent(cp)
	if s.Score < 0.8 {
		t.Fatalf("healthy page scored %v, want >= 0.8: %+v", s.Score, s.Features)
	}
}

// A parked page (K1) tanks but the scorer cites the K1 feature.
func TestScoreContentParked(t *testing.T) {
	cp := ContentPage{Page: Page{
		Domain: "forsale.example", Title: "forsale.example",
		Text:     "This domain is for sale. Buy this domain today. Make an offer.",
		WordCount: 12,
	}}
	s := ScoreContent(cp)
	if s.Score > 0.45 {
		t.Fatalf("parked page scored %v, want <= 0.45: %+v", s.Score, s.Features)
	}
	has := false
	for _, r := range s.Reasons {
		if r == "junk:parked" {
			has = true
		}
	}
	if !has {
		t.Fatalf("expected junk:parked reason, got %v", s.Reasons)
	}
}

// Keyword stuffing alone cannot condemn without corroboration.
func TestScoreContentStuffingBounded(t *testing.T) {
	text := strings.Repeat("cheap loans ", 30) + strings.Repeat("apply now for our services and read the details carefully. ", 20)
	cp := ContentPage{Page: Page{Domain: "x.com", Title: "Loans", Text: text, WordCount: 400}}
	s := ScoreContent(cp)
	if s.Score < 0.4 {
		t.Fatalf("stuffing alone dropped to %v, want >= 0.4: %+v", s.Score, s.Features)
	}
	if s.Score >= 1.0 {
		t.Fatalf("stuffing had no effect: %+v", s.Features)
	}
}

// Score stays in [0,1] for degenerate input.
func TestScoreContentBounds(t *testing.T) {
	cp := ContentPage{Page: Page{Domain: "x.com", Text: "", WordCount: 0}, NearDupSize: 50}
	s := ScoreContent(cp)
	if s.Score < 0 || s.Score > 1 {
		t.Fatalf("score out of bounds: %v", s.Score)
	}
}

// Zero-valued ContentPage equals a plain Page (additive fields are neutral).
func TestScoreContentNeutralFields(t *testing.T) {
	p := Page{Domain: "x.com", Title: "T", Text: "some real content here with words", WordCount: 6}
	a := ScoreContent(ContentPage{Page: p})
	b := ScoreContent(ContentPage{Page: p, HeadingCount: 0, ImageCount: 0, NearDupSize: 0})
	if a.Score != b.Score {
		t.Fatalf("zero fields changed score: %v vs %v", a.Score, b.Score)
	}
}
