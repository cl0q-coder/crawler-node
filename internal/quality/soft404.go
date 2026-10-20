// K2 (soft-404 / thin-content detection): pages that return HTTP 200 but
// carry no real content — empty bodies, "page not found" templates, "coming
// soon" placeholders, default web-server pages. Same additive pattern as K1:
// a pure detector, no crawl-behavior change. The pre-index gate wire-in is a
// separate PR and holds for Claude's final say (Track K, crawler-node #284).
//
// Design rules (same as K1):
//   - DetectSoft404 is pure: no I/O, no DB, no DNS. One pass over strings.
//   - Every verdict carries auditable reasons ("text:page-not-found") and a
//     score in [0,1]. The labeled-sample measurement (Track K acceptance)
//     sets the threshold; nothing in the gate may act on an unmeasured
//     verdict (SafeSearch lesson: never over-filter legit sites).
//   - Guards against the classic false positives, in order of danger:
//     1. JS-rendered legit pages show empty raw text. Empty text alone
//     (0.55) stays UNDER threshold — corroboration is required.
//     2. Articles *about* 404 handling mention "page not found" in prose.
//     Body-text phrase matches count HALF weight; the title carries the
//     signal on real soft-404 templates.
//     3. "Coming soon" appears in legit prose ("our new store is coming
//     soon"). Placeholder phrases are low-weight and need corroboration.
//     4. Legit sparse pages (contact stubs) are thin but real. The thin
//     corroboration only applies when the title is empty or ~= domain.
package quality

import "strings"

// Soft404Threshold is the score at which a page counts as soft-404 /
// thin-content junk. Tunable, not sacred; the labeled-sample measurement
// (Track K acceptance) sets it.
const Soft404Threshold = 0.60

// emptyTextScore is the single biggest signal — but deliberately kept under
// the threshold so a JS-rendered legit page (empty raw text, real title)
// cannot be condemned on one signal alone.
const emptyTextScore = 0.55

// thinWordFloor is the word-count ceiling for the thin-corroboration signal.
// thinBonus applies only when the title is empty or ~= domain, so a short
// real page titled "Contact" or "Menu" earns no bonus.
const thinWordFloor = 25
const thinBonus = 0.20

// soft404Phrases are boilerplate strings from soft-404 templates (CMS
// "page not found" themes, server error pages served with 200). A match in
// the <title> counts full weight; a match only in the body text counts half
// (see the "article about 404s" fixture — body mentions must not condemn).
var soft404Phrases = []struct {
	phrase string
	weight float64
	reason string
}{
	{"404 - page not found", 0.45, "text:404-page-not-found"},
	{"oops! that page can't be found", 0.40, "text:oops-page-cant-be-found"},
	{"page not found", 0.40, "text:page-not-found"},
	{"this page does not exist", 0.35, "text:this-page-does-not-exist"},
	{"the page you are looking for", 0.35, "text:page-you-are-looking-for"},
	{"error 404", 0.35, "text:error-404"},
	{"sorry, we couldn't find that page", 0.35, "text:couldnt-find-that-page"},
	{"page cannot be found", 0.30, "text:page-cannot-be-found"},
}

// placeholderPhrases are boilerplate from placeholder and default pages.
// Corroborating phrases count half weight in body text (thin pages have no
// real prose, so a thin page whose title is empty or ~= domain AND carries a
// placeholder phrase is a template — see thinPlaceholderBonus). The
// "default web site page" titles are the IIS/Apache defaults: no legit page
// is ever titled that, so a title match condemns on its own.
var placeholderPhrases = []struct {
	phrase string
	weight float64
	reason string
}{
	{"default web site page", 0.60, "text:default-web-site-page"},
	{"default website page", 0.60, "text:default-website-page"},
	{"this site is a placeholder", 0.25, "text:site-is-placeholder"},
	{"content goes here", 0.20, "text:content-goes-here"},
	{"under construction", 0.15, "text:under-construction"},
	{"coming soon", 0.15, "text:coming-soon"},
}

// thinPlaceholderBonus fires on the classic placeholder template: a thin
// page with an empty or bare-domain title that carries placeholder
// boilerplate. Thin + bare title alone is never enough (see the contact-stub
// fixture); placeholder prose on a real, titled page is never enough (see
// the legit "coming soon" fixture). The combination is the template.
const thinPlaceholderBonus = 0.45

// normalizeApos folds curly quotes/apostrophes so templates that use ' or '
// match the same phrases as ones that use '.
func normalizeApos(s string) string {
	r := strings.NewReplacer("\u2018", "'", "\u2019", "'", "\u201c", "\"", "\u201d", "\"")
	return r.Replace(s)
}

// titleIsBare reports whether the title is empty or effectively the domain
// itself (a bare placeholder title, not a real page title).
func titleIsBare(domain, title string) bool {
	t := strings.ToLower(strings.TrimSpace(title))
	if t == "" {
		return true
	}
	d := strings.ToLower(strings.TrimSpace(domain))
	t = strings.TrimPrefix(t, "www.")
	d = strings.TrimPrefix(d, "www.")
	return t == d
}

// DetectSoft404 scores a crawled page for soft-404 / thin-content junk: a
// 200-status page with empty body, a "not found" template, or placeholder
// boilerplate. It never reads anything but the Page it is given.
func DetectSoft404(p Page) Verdict {
	v := Verdict{Score: 0}
	title := strings.ToLower(normalizeApos(p.Title))
	text := strings.ToLower(normalizeApos(p.Text))

	add := func(w float64, reason string) {
		v.Score += w
		v.Reasons = append(v.Reasons, reason)
	}

	// 1. Empty 200: no visible text at all. Strong, but sub-threshold alone.
	if strings.TrimSpace(p.Text) == "" && p.WordCount == 0 {
		add(emptyTextScore, "empty:zero-text")
	}

	// 2. Soft-404 templates: title match = full weight, body-only = half.
	for _, sp := range soft404Phrases {
		switch {
		case strings.Contains(title, sp.phrase):
			add(sp.weight, sp.reason)
		case strings.Contains(text, sp.phrase):
			add(sp.weight/2, sp.reason+"(body)")
		}
	}

	// 3. Placeholder boilerplate: same title/body split. Remember whether any
	// placeholder phrase fired, for the thin-template composite below.
	placeholderHit := false
	for _, pp := range placeholderPhrases {
		switch {
		case strings.Contains(title, pp.phrase):
			add(pp.weight, pp.reason)
			placeholderHit = true
		case strings.Contains(text, pp.phrase):
			add(pp.weight/2, pp.reason+"(body)")
			placeholderHit = true
		}
	}

	// 4. Thin corroboration: a stub page whose title is empty or bare-domain.
	// A thin placeholder template (bare title + boilerplate) is the classic
	// junk shape; thin + bare title with NO template signal stays silent.
	if thinBare := p.WordCount < thinWordFloor && titleIsBare(p.Domain, p.Title); thinBare {
		add(thinBonus, "thin:low-word-count-bare-title")
		if placeholderHit {
			add(thinPlaceholderBonus, "template:thin-placeholder")
		}
	}

	if v.Score > 1 {
		v.Score = 1
	}
	v.Soft404 = v.Score >= Soft404Threshold
	return v
}
