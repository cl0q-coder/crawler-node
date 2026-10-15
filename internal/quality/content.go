// K4 (content-quality score): a cheap per-page quality score to rank-down
// or exclude junk; feeds Track E (ranking experiments). Same additive
// pattern as K1/K2: a pure scorer with auditable features, no crawl-behavior
// change. The pre-index gate wire-in is a separate PR and holds for Claude's
// final say (Track K, crawler-node #284).
//
// Design rules (same as K1/K2):
//   - ScoreContent is pure: no I/O, no DB, no DNS. One pass over strings.
//   - Score in [0,1]: 1 = high-quality content, 0 = junk. The score is a
//     blend of junk features (negative) and positive content features.
//   - Every score carries an auditable feature breakdown and reasons.
//     Thresholds are TUNABLE and unmeasured until the E-b labeled sample
//     measurement (SafeSearch lesson: no single feature may condemn a legit
//     page — corroboration is required).
//   - K1/K2 verdicts are input FEATURES, not duplicates: DetectParked and
//     DetectSoft404 scores feed the junk side as strong priors. K3's
//     neardup cluster size is an optional feature once #298 merges; the
//     scorer must not hard-fail without it (see NearDupClusters).
package quality

import (
	"strings"
	"unicode"
)

// QualityScore is the K4 answer for one page.
type QualityScore struct {
	Score    float64            // overall content quality in [0,1]; 1 = high quality
	Features map[string]float64 // auditable per-feature contributions (negative = junk)
	Reasons  []string           // auditable feature codes, e.g. "junk:keyword-stuffing"
}

// ContentPage extends Page with the optional structural counts the K4
// scorer uses. All fields are zero-neutral: a page without the counts
// scores exactly as a plain Page does.
type ContentPage struct {
	Page
	HeadingCount int // h1-h3 headings on the page
	ImageCount   int // <img> tags
	NearDupSize  int // K3 cluster size this page belongs to (1 = unique; 0 = unknown)
}

// ScoreContent scores a crawled page's content quality.
// Pure: reads only the page it is given. Cheap: one string pass plus the
// K1/K2 detector calls (which share the same input text).
func ScoreContent(cp ContentPage) QualityScore {
	s := QualityScore{Score: 1.0, Features: map[string]float64{}, Reasons: []string{}}
	p := cp.Page

	// --- junk priors from K1/K2 (strong, but never condemn alone) ---
	if v := DetectParked(p); v.Parked {
		apply(&s, -0.45, "junk:parked")
	} else if v.Score >= 0.3 {
		apply(&s, -v.Score*0.4, "junk:parked-partial")
	}
	if v := DetectSoft404(p); v.Soft404 {
		apply(&s, -0.45, "junk:soft404")
	} else if v.Score >= 0.3 {
		apply(&s, -v.Score*0.4, "junk:soft404-partial")
	}
	if cp.NearDupSize >= nearDupJunkSize {
		apply(&s, -0.30, "junk:near-dup-cluster")
	}

	// --- keyword stuffing: max repeated-word density ---
	if density := maxWordDensity(p.Text); density >= stuffingDensity {
		apply(&s, -0.35, "junk:keyword-stuffing")
	} else if density >= stuffingDensity/2 {
		apply(&s, -0.15, "junk:keyword-stuffing-partial")
	}

	// --- ad-link density ---
	if p.WordCount > 0 {
		if d := float64(p.OutboundAdLinks) * 100 / float64(p.WordCount); d >= adDensityJunk {
			apply(&s, -0.25, "junk:ad-density")
		}
	}

	// --- all-caps / exclamation shouting ---
	if r := shoutRatio(p.Text); r >= shoutJunk {
		apply(&s, -0.20, "junk:shouting")
	}

	// --- ultra-thin text corroboration (title empty or ~= domain only) ---
	thin := p.WordCount < thinTextFloor
	if thin && (p.Title == "" || domainishTitle(p.Title, p.Domain)) {
		apply(&s, -0.25, "junk:ultra-thin")
	}

	// --- single ad frame (K1's structural signal, cheaper weight here) ---
	if p.SingleAdFrame {
		apply(&s, -0.15, "junk:single-ad-frame")
	}

	// --- positive features ---
	if p.WordCount >= healthyTextMin && p.WordCount <= healthyTextMax {
		apply(&s, +0.10, "quality:healthy-length")
	}
	if cp.HeadingCount >= 2 {
		apply(&s, +0.10, "quality:structure")
	}

	if s.Score < 0 {
		s.Score = 0
	}
	if s.Score > 1 {
		s.Score = 1
	}
	return s
}

const (
	// nearDupJunkSize is the K3 cluster size at which near-dup membership
	// starts counting against quality (spam networks, mirror farms).
	nearDupJunkSize = 5
	// stuffingDensity is the single-word repetition density that counts as
	// keyword stuffing. Deliberately high: legit repetitive lists (song
	// lyrics, legal boilerplate) must not trip it alone — and even tripped,
	// stuffing alone cannot drop a page below ~0.4 without corroboration.
	stuffingDensity = 0.08
	// adDensityJunk is ad links per 100 words at which a page looks like an
	// ad farm rather than content.
	adDensityJunk = 8.0
	// shoutJunk is the fraction of alpha characters that are uppercase (or
	// the fraction of sentences ending in !, whichever) that counts as
	// shouting.
	shoutJunk      = 0.45
	thinTextFloor  = 30
	healthyTextMin = 150
	healthyTextMax = 5000
)

func apply(s *QualityScore, delta float64, reason string) {
	s.Features[reason] = delta
	s.Reasons = append(s.Reasons, reason)
	s.Score += delta
}

// maxWordDensity returns the highest single-word share of total words.
func maxWordDensity(text string) float64 {
	words := strings.Fields(strings.ToLower(text))
	if len(words) == 0 {
		return 0
	}
	counts := map[string]int{}
	max := 0
	for _, w := range words {
		counts[w]++
		if counts[w] > max {
			max = counts[w]
		}
	}
	return float64(max) / float64(len(words))
}

// shoutRatio returns the fraction of letters that are uppercase, mixed
// with the fraction of sentence ends that are exclamations.
func shoutRatio(text string) float64 {
	letters, upper := 0, 0
	for _, r := range text {
		if unicode.IsLetter(r) {
			letters++
			if unicode.IsUpper(r) {
				upper++
			}
		}
	}
	if letters == 0 {
		return 0
	}
	return float64(upper) / float64(letters)
}

// domainishTitle reports whether the title is empty or is just the domain
// (e.g. "example.com" or "Example.com - Home" patterns carry no content).
func domainishTitle(title, domain string) bool {
	t := strings.ToLower(strings.TrimSpace(title))
	d := strings.ToLower(domain)
	return t == d || t == "home" || strings.HasPrefix(t, d+" ") || strings.HasPrefix(t, d+" -")
}
