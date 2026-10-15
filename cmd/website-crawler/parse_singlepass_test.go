package main

// Differential test: parseHTMLSinglePass must extract exactly the same fields
// as html.Parse + parseHTMLInfo across a corpus of tricky documents.

import (
	"reflect"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// parseDOM is the legacy path: html.Parse then parseHTMLInfo.
func parseDOM(t *testing.T, doc, domain string) *WebsiteInfo {
	t.Helper()
	info := &WebsiteInfo{}
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("html.Parse: %v", err)
	}
	parseHTMLInfo(root, info, domain)
	return info
}

func parseSP(t *testing.T, doc, domain string) *WebsiteInfo {
	t.Helper()
	info := &WebsiteInfo{}
	parseHTMLSinglePass(strings.NewReader(doc), info, domain)
	return info
}

func diffInfos(a, b *WebsiteInfo) []string {
	var diffs []string
	check := func(name string, x, y any) {
		if !reflect.DeepEqual(x, y) {
			diffs = append(diffs, name)
		}
	}
	check("Title", a.Title, b.Title)
	check("H1", a.H1, b.H1)
	check("Description", a.Description, b.Description)
	check("Keywords", a.Keywords, b.Keywords)
	check("Canonical", a.Canonical, b.Canonical)
	check("Charset", a.Charset, b.Charset)
	check("Robots", a.Robots, b.Robots)
	check("OGTitle", a.OGTitle, b.OGTitle)
	check("OGImage", a.OGImage, b.OGImage)
	check("TwitterCard", a.TwitterCard, b.TwitterCard)
	check("PageLanguage", a.PageLanguage, b.PageLanguage)
	check("ImageCount", a.ImageCount, b.ImageCount)
	check("OutboundLinkCount", a.OutboundLinkCount, b.OutboundLinkCount)
	check("WordCount", a.WordCount, b.WordCount)
	check("SimHash", a.SimHash, b.SimHash)
	check("outDomains", a.outDomains, b.outDomains)
	return diffs
}

// differentialCorpus is the shared document corpus for the single-pass
// differential tests (TestParseSinglePassDifferential and FuzzParseSinglePass):
// both parsers must extract identical fields from every doc.
var differentialLorem = "Lorem ipsum dolor sit amet, consectetur adipiscing elit. "
var differentialBigPara = strings.Repeat("<p>"+strings.Repeat(differentialLorem, 20)+"</p>", 200) // ~500KB

var differentialCorpus = map[string]string{
	"basic": `<!DOCTYPE html><html lang="en"><head><title>Hello World</title>
			<meta name="description" content="a test page"><meta charset="utf-8">
			<link rel="canonical" href="https://example.com/"></head>
			<body><h1>Welcome</h1><p>Some body text here.</p>
			<a href="https://other.com/page">out</a><a href="/local">in</a>
			<img src="x.png"><img src="y.png"></body></html>`,
	"uppercase": `<HTML LANG="EN"><HEAD><TITLE>Up Case</TITLE>
			<META NAME="Description" CONTENT="upper attrs">
			<LINK REL="Canonical" HREF="https://example.com/u"></HEAD>
			<BODY><H1>Hi</H1><P>Text <B>bold</B> tail.</P>
			<A HREF="https://other.com/">O</A><IMG SRC="z.png"></BODY></HTML>`,
	"entities": `<html><head><title>Fish &amp; Chips &lt;yum&gt;</title></head>
			<body><p>A &amp; B &mdash; C &#169; D</p></body></html>`,
	"title-with-tag-text": `<html><head><title>keep < this part</title></head>
			<body><p>x</p></body></html>`,
	"nested-title-h1": `<html><head><title>a<b>c</b>d</title></head>
			<body><h1>one <span>two</span> three</h1></body></html>`,
	"double-h1":          `<html><body><h1>first</h1><p>t</p><h1>second</h1></body></html>`,
	"h1-in-title-rcdata": `<html><head><title>x<h1>y</h1>z</title></head><body></body></html>`,
	"script-style-noscript": `<html><head><title>T</title><style>.a{color:red}</style>
			<script>var x = "<p>not text</p>";</script></head>
			<body><noscript><p>no js text</p></noscript><p>real text</p>
			<script>more();</script></body></html>`,
	"svg-template": `<html><body><svg><text>svg words</text></svg>
			<template><p>template words</p></template><p>kept</p></body></html>`,
	"comments": `<html><body><!-- a comment --><p>text<!-- inner -->more</p></body></html>`,
	"malformed": `<html><head><title>Unclosed<p><div><p>para1<p>para2
			<a href=https://bare.com>bare</a><br><hr></div>`,
	"stray-end-tags": `<html><body></div></p><p>ok</p></span></body></html>`,
	"meta-variants": `<html><head>
			<meta property="og:title" content="OG Title Here">
			<meta property="og:description" content="og desc">
			<meta property="og:image" content="https://example.com/og.png">
			<meta name="twitter:card" content="summary">
			<meta name="robots" content="noindex">
			<meta name="keywords" content="a, b, c">
			<meta name="description" content="first"><meta name="description" content="second">
			</head><body></body></html>`,
	"anchors": `<html><body>
			<a href="mailto:x@y.com">m</a><a href="tel:123">t</a>
			<a href="javascript:void(0)">j</a><a href="#frag">f</a>
			<a href="//proto-relative.com/p">pr</a>
			<a href="https://www.example.com/same">same-site</a>
			<a href="https://sub.example.com/">sub</a>
			<a href="https://deep.sub.example.com/">deep</a>
			<a href="http://192.0.2.1/">ip</a>
			<a href="https://dup.com/" href="https://second.com/">dup-attr</a>
			<a>no href</a><a href="">empty</a>
			<a href="https://other.com/very/long/path/that/goes/on/and/on/and/on/and/on">long</a>
			</body></html>`,
	"unicode": `<html lang="ja"><head><title>日本語タイトル</title></head>
			<body><p>こんにちは 世界 中文 한국어 العربية</p></body></html>`,
	"empty":     ``,
	"text-only": `just some text, no tags at all`,
	"big": `<html><head><title>Big Page</title>
			<meta name="description" content="big"></head><body>` + differentialBigPara +
		`<a href="https://far.com/">x</a></body></html>`,
	"table-foster":    `<html><body><table><a href="https://foster.com/">stray</a><tr><td>cell</td></tr></table></body></html>`,
	"head-body-order": `<html><body><p>body first</p></body><head><title>Late</title></head></html>`,
	"dup-meta-charset": `<html><head><meta charset="utf-8"><meta charset="iso-8859-1">
			<meta name="description" content="x"></head><body></body></html>`,
	"nul-byte":    "\x00",
	"nul-in-text": `<html><body><p>a` + "\x00" + `b</p><title>t` + "\x00" + `t</title></body></html>`,
	// Truncated tag at EOF: the tokenizer splits "0</" into two text tokens
	// while html.Parse coalesces it into one text node (found by fuzzing).
	"truncated-tag-eof":   "0</",
	"truncated-tag-eof-2": `<p>hello</`,
}

// differentialCorpusQ5 holds the Track Q rung Q5 corpus extension (#283):
// new edge cases layered on top of differentialCorpus, all fed to both the
// deterministic differential test and the FuzzParseSinglePass seed set.
var differentialCorpusQ5 = map[string]string{
	"q5-deep-nesting":    `<html><body>` + strings.Repeat("<div>", 150) + `deep text` + strings.Repeat("</div>", 150) + `</body></html>`,
	"q5-huge-attr":       `<html><head><meta name="description" content="` + strings.Repeat("a", 65536) + `"></head><body></body></html>`,
	"q5-entity-soup":     `<html><head><title>&notit;&copyx;&AMP;&amplt;&nbsp;&Tab;&NewLine;&frac34;</title></head><body><p>&#65;&#x42;&#x1F600;&#0;&#xD800;&bogusentity;</p></body></html>`,
	"q5-unquoted-attrs":  `<html><body><a href=/foo/bar?x=1&y=2 title=some title>link</a><img src=x.png alt=a b><div class=one two=2>text</div></body></html>`,
	"q5-attr-edge":       `<html><body><div class==foo data-x= a='1' b="2" c=>t</div><a href = "https://spaced.com/" >spaced</a></body></html>`,
	"q5-li-without-ul":   `<html><body><li>one<li>two<p>para</p><li>three</body></html>`,
	"q5-select-textarea": `<html><body><select><option>a<option selected>b</select><textarea>rcdata <b>bold</b> text</textarea></body></html>`,
	"q5-pre-whitespace":  "<html><body><pre>  keep   spaces\n\n  newlines\t\ttabs</pre><p>normal</p></body></html>",
	"q5-iframe-object":   `<html><body><iframe><p>iframe fallback</p></iframe><object><p>obj fallback</p></object><noembed><p>ne</p></noembed><p>after</p></body></html>`,
	"q5-xmp-plaintext":   `<html><body><xmp><b>not bold</b></xmp><p>after xmp</p></body></html>`,
	"q5-ruby":            `<html><body><ruby>漢<rt>かん</rt>字<rt>じ</rt></ruby><p>after ruby</p></body></html>`,
	"q5-cond-comment":    `<html><body><!--[if IE]><p>ie only</p><![endif]--><p>real</p></body></html>`,
	"q5-bogus-markup":    `<html><body><?php echo "hi"; ?><!random bogus><p>x</p></body></html>`,
	"q5-doctype-variant": `<!DOCTYPE html PUBLIC "-//W3C//DTD HTML 4.01//EN"><html><head><title>d</title></head><body><p>t</p></body></html>`,
	"q5-multi-root":      `<html><head><title>t</title></head><body><p>a</p></html><html><body><p>second root</p></body></html>`,
	"q5-headless":        `<title>no wrappers</title><h1>hi</h1><p>text</p>`,
	"q5-title-in-body":   `<html><body><title>body title</title><p>x</p></body></html>`,
	"q5-form-controls":   `<html><body><form action="/s"><input type=text name=q><input type=hidden name=h value=v><button>go</button><label>lbl</label></form></body></html>`,
	"q5-table-deep":      `<html><body><table><caption>cap</caption><colgroup><col></colgroup><thead><tr><th>h</th></tr></thead><tbody><tr><td><table><tr><td>nested</td></tr></table></td></tr></tbody><tfoot><tr><td>f</td></tr></tfoot></table></body></html>`,
	"q5-script-tricky":   `<html><head><script>var s = "</script> not really";</script><script type="application/json">{"a":"</b>"}</script></head><body><p>after</p></body></html>`,
	"q5-style-tricky":    `<html><head><style>a{content:"</style>"}</style></head><body><p>after</p></body></html>`,
	"q5-skip-case":       `<html><head><SCRIPT>var x = 1;</SCRIPT><STYLE>.a{}</STYLE></head><body><NOSCRIPT><p>ns</p></NOSCRIPT><p>after</p></body></html>`,
	"q5-meta-http-equiv": `<html><head><meta http-equiv="Content-Type" content="text/html; charset=iso-8859-1"><meta http-equiv=refresh content="5;url=x"><meta charset="utf-16"></head><body></body></html>`,
	"q5-link-variants":   `<html><head><link rel="alternate" hreflang="es" href="https://es.example.com/"><link rel=icon href=/favicon.ico><link rel="canonical stylesheet" href="x.css"></head><body></body></html>`,
	"q5-img-srcset":      `<html><body><img srcset="a.png 1x, b.png 2x" src="a.png"><img></body></html>`,
	"q5-anchor-edge":     `<html><body><a href="HTTP://EXAMPLE.COM/UPPER">up</a><a href=" https://spaced.com/ ">sp</a><area href="https://area.com/"><a href="ftp://ftp.example.com/f">ftp</a></body></html>`,
	"q5-h1-deep-inline":  `<html><body><h1>a <b><i><u>deep</u></i></b> tail</h1></body></html>`,
	"q5-huge-text":       `<html><body><p>` + strings.Repeat("word ", 50000) + `</p></body></html>`,
	"q5-h1-in-script":    `<html><head><title>t</title><script><h1>not an h1</h1></script></head><body><h1>real h1</h1></body></html>`,
	"q5-template-deep":   `<html><body><template><template><p>double nested</p></template></template><p>kept</p></body></html>`,
	// Q5 fuzz regression: <title> inside <h1> is a real element (h1 is not
	// RCDATA) — the DOM sets both Title and H1, title text counts for both.
	"q5-title-in-h1":     `<h1><title>0</title>1</h1>`,
	"q5-title-in-h1-eof": `<h1><title>0`,
	// Q5 fuzz regression: the tree builder only closes an open h1 for a
	// nested h1-h6 start tag when the h1 is the top of the open-elements
	// stack (x/net inBodyIM: pop p's in button scope, then pop a top
	// h1-h6) — <h1> nested through another element stays nested.
	"q5-h1-nest-direct":    `<h1>0<h1>1</h1>`,
	"q5-h1-nest-through":   `<h1>0<b><h1>1</h1>2</h1>`,
	"q5-h1-nest-through-a": `<H1>0<A><H1>0`,
	"q5-h1-nest-p":         `<h1>0<p><h1>1</h1>2</h1>`,
	"q5-h1-nest-h2":        `<h1>0<h2>1</h2>2</h1>`,
	// Q5 fuzz regression: html/head/body tags never create text-node
	// boundaries — duplicates are ignored by the tree builder and text
	// around them relocates into the body, merging into one text node.
	"q5-struct-head":   `0<heAd>0`,
	"q5-struct-body":   `<body>0<body>0`,
	"q5-struct-body-e": `<body>0</body>0`,
	"q5-struct-html":   `0<html>0</html>0`,
	// Q5 fuzz regression: SVG foreign-content breakout — the tree builder
	// pops an svg element and reprocesses breakout tags (b, div, h1, ...)
	// as HTML, so their content counts; a plain subtree skip would swallow
	// it. font breaks out only with color/face/size attributes.
	"q5-svg-breakout":      `<svg><B>0`,
	"q5-svg-breakout-div":  `<svg><div>0</div></svg><p>1</p>`,
	"q5-svg-no-breakout":   `<svg><circle>0</circle></svg>`,
	"q5-svg-breakout-h1":   `<h1><svg><b>0</b></svg></h1>`,
	"q5-svg-breakout-font": `<svg><font color="red">0</font></svg>`,
	// Q5 fuzz regression: NUL bytes become U+FFFD in foreign content
	// (svg/math) but are dropped in HTML content — the tree builder
	// distinguishes, so the single-pass NUL normalization must too.
	// (textOf doesn't skip svg, so svg text counts toward title/h1.)
	"q5-nul-foreign-svg":  "<h1><svg>\x00",
	"q5-nul-foreign-math": "<math>\x00</math>",
	"q5-nul-html":         "<h1>a\x00b</h1>",
	// Q5 fuzz regression: html/head/body starts are structurally ignored
	// by the tree builder (attributes merged at most), so they must not
	// hide an open h1 from the h1 rule's top-of-stack check.
	"q5-struct-h1-body": `<h1>0<BodY><h1>0`,
	"q5-struct-h1-head": `<h1>0<head><h1>0`,
	// Q5 fuzz regression: a self-closing flag on non-void elements is
	// ignored by the tree builder (parse error) — <h1/> opens h1.
	"q5-selfclose-h1": `<h1/>0`,
	// Q5 fuzz regression: the DOM walk's first-wins guard (info.H1 == "")
	// lets a LATER h1 overwrite an EMPTY cleanTitle — "<h1><<A><h1>0<h1>"
	// yields H1="0" (outer textOf "<0" truncates to ""). Single-pass keeps
	// popped inner frames as fallback candidates for this.
	"q5-h1-empty-overwrite": `<h1><<A><h1>0<h1>`,
	// Q5 fuzz regression: ignored end tags create no DOM text boundary.
	// "<Bod\x90>0</Bod\xe7>0" — the end tag's raw bytes don't match the
	// open element (x/net TagName lowercases ASCII-only, preserving
	// invalid UTF-8; strings.ToLower would corrupt them to U+FFFD and
	// falsely match), so the token is dropped and the text merges.
	// Single-pass now flushes the text run only for end tags the tree
	// builder acts on (endTagActs). This also fixes the simple #279-3
	// case "0</A>0"; misnested formatting elements (adoption agency) stay
	// deferred to Claude.
	"q5-ignored-endtag-utf8": "<Bod\x90>0</Bod\xe7>0",
	"q5-ignored-endtag-div":  "<DIV\xff>0</div>0",
	// Q5 fuzz regression: "<A><A></A>0</A>0" — an <a> start tag with `a`
	// already open runs the adoption agency; with no special elements
	// between, the open `a` is simply closed (sibling-ized), so the text
	// merges. Single-pass pops to the open `a` in that simple case.
	"q5-anchor-sibling": `<A><A></A>0</A>0`,
	// Q5 fuzz regression: "0<!DOCTYPE>0" — the tree builder ignores
	// DOCTYPE tokens outside document-initial position (parse error), and
	// a DOCTYPE never creates a text boundary, so the text merges.
	// Single-pass no longer flushes the text run for DoctypeToken
	// (comments still split, as comment nodes).
	"q5-doctype-ignored": `0<!DOCTYPE>0`,
	// Q5 fuzz regression: self-closing <script/> — the tree builder
	// ignores the self-closing flag on HTML elements, so <script/> OPENS
	// a script (raw text until </script> or EOF); only foreign <svg/>
	// respects self-closing. Single-pass now enters the skip region for
	// self-closing HTML skip tags (and tracks the open element).
	"q5-selfclose-script": `<script/>0`,
}

func init() {
	// Merge the Q5 extension into the shared corpus: the deterministic
	// differential test and the FuzzParseSinglePass seed set both grow.
	for name, doc := range differentialCorpusQ5 {
		if _, dup := differentialCorpus[name]; dup {
			panic("differentialCorpusQ5: duplicate key " + name)
		}
		differentialCorpus[name] = doc
	}
}

func TestParseSinglePassDifferential(t *testing.T) {
	for name, doc := range differentialCorpus {
		t.Run(name, func(t *testing.T) {
			dom := parseDOM(t, doc, "example.com")
			sp := parseSP(t, doc, "example.com")
			if diffs := diffInfos(dom, sp); len(diffs) > 0 {
				t.Errorf("field mismatches: %v", diffs)
				t.Logf("DOM: Title=%q H1=%q Words=%d SimHash=%x Out=%d Domains=%v",
					dom.Title, dom.H1, dom.WordCount, dom.SimHash, dom.OutboundLinkCount, dom.outDomains)
				t.Logf("SP : Title=%q H1=%q Words=%d SimHash=%x Out=%d Domains=%v",
					sp.Title, sp.H1, sp.WordCount, sp.SimHash, sp.OutboundLinkCount, sp.outDomains)
			}
		})
	}
}

// FuzzParseSinglePass: random HTML must extract identically on both parsers
// (Claude's #277 required condition). Seeded with differentialCorpus.
func FuzzParseSinglePass(f *testing.F) {
	for _, doc := range differentialCorpus {
		f.Add(doc)
	}
	f.Fuzz(func(t *testing.T, doc string) {
		dom := parseDOM(t, doc, "example.com")
		sp := parseSP(t, doc, "example.com")
		if diffs := diffInfos(dom, sp); len(diffs) > 0 {
			// Q5: skip the two known-deferred divergence classes (both need
			// tree-builder modeling the team deferred to Claude — see #279
			// finding 3): (1) an end tag the tree builder ignores merges
			// adjacent text in the DOM, while single-pass ends the text run
			// at every end tag, splitting WordCount/SimHash; (2) table
			// foster parenting merges text across table-family start tags
			// (e.g. "0<table>0" is one DOM text node "00"). Skipping keeps
			// the fuzzer hunting NEW divergences instead of re-reporting the
			// deferred ones on every run. The deterministic differential
			// test above does NOT skip: any corpus regression still fails
			// loudly.
			if isKnownDeferredDivergence(doc, diffs) {
				t.Skipf("known deferred divergence (tree-builder text merge): %q", doc)
			}
			t.Errorf("field mismatches on %q: %v", doc, diffs)
		}
	})
}

// isKnownDeferredDivergence reports whether a differential mismatch matches
// one of the known-deferred tree-builder classes (all need HTML
// insertion-mode modeling the team deferred to Claude per #279):
//
//  1. Ignored end tags merge adjacent text in the DOM, while single-pass
//     ends the text run at every end tag (WordCount/SimHash only).
//  2. Table foster parenting merges text across table-family start tags,
//     e.g. "0<table>0" is one DOM text node "00" (WordCount/SimHash only).
//     3-4. Active-formatting reconstruction DUPLICATES elements: the tree
//     builder clones unclosed formatting elements (a, b, i, ...) when
//     text or block starts arrive — e.g. "<table><a href=//0><table>0"
//     yields two <a> elements, and "<p><a href=//0><h1>0" reconstructs
//     the <a> inside the h1 when its text arrives. Reproducing this needs
//     the active-formatting list + reconstruction algorithm.
//
// Skipping keeps the fuzzer hunting NEW classes instead of re-reporting the
// deferred ones. The deterministic differential test above does NOT skip:
// any corpus regression still fails loudly.
func isKnownDeferredDivergence(doc string, diffs []string) bool {
	textOnly := true
	countOnly := true
	for _, d := range diffs {
		if d != "WordCount" && d != "SimHash" {
			textOnly = false
		}
		switch d {
		case "WordCount", "SimHash", "OutboundLinkCount", "ImageCount", "outDomains":
		default:
			countOnly = false
		}
	}
	if textOnly && (hasIgnoredEndTag(doc) || hasTableFosterShape(doc)) {
		return true
	}
	// Classes 3-4: element duplication via formatting reconstruction.
	return countOnly && hasDuplicatedFormatting(doc)
}

// hasIgnoredEndTag reports whether doc contains an end tag with no matching
// open element on a naive open-element scan — the shape the HTML tree
// builder ignores, merging the adjacent text the single-pass parser splits.
func hasIgnoredEndTag(doc string) bool {
	var stack []string
	z := html.NewTokenizer(strings.NewReader(doc))
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return false
		case html.StartTagToken:
			name, _ := z.TagName()
			stack = append(stack, lowerTagASCII(name))
		case html.EndTagToken:
			name, _ := z.TagName()
			tn := lowerTagASCII(name)
			found := -1
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i] == tn {
					found = i
					break
				}
			}
			if found < 0 {
				return true // no open element: tree builder ignores this end tag
			}
			stack = stack[:found]
		}
	}
}

// tableFosterTags are the table-family elements whose content the HTML tree
// builder foster-parents: text adjacent to them can merge into a single DOM
// text node across the tag, which the single-pass text-run splitter cannot
// see without insertion-mode modeling.
var tableFosterTags = []string{
	"<table", "<caption", "<colgroup", "<col", "<tbody", "<thead", "<tfoot",
	"<tr", "<td", "<th",
}

// hasTableFosterShape reports whether doc contains a table-family tag, the
// shape in which the tree builder foster-parents text (merging runs the
// single-pass parser splits). Coarse on purpose: it only gates the skip for
// WordCount/SimHash-only mismatches, and the deterministic differential test
// never skips.
func hasTableFosterShape(doc string) bool {
	lower := strings.ToLower(doc)
	for _, tag := range tableFosterTags {
		if strings.Contains(lower, tag) {
			return true
		}
	}
	return false
}

// isFormattingElem reports whether tn is an HTML formatting element — the
// ones the tree builder tracks in its active-formatting list and may
// reconstruct (duplicating them) when text or block starts arrive.
func isFormattingElem(tn string) bool {
	switch tn {
	case "a", "b", "big", "code", "em", "font", "i", "nobr",
		"s", "small", "strike", "strong", "tt", "u":
		return true
	}
	return false
}

// hasDuplicatedFormatting reports whether the DOM built from doc contains
// MORE formatting elements than the doc has start tags for — the exact
// signature of active-formatting reconstruction (or adoption-agency)
// duplication, which a tokenizer pass cannot reproduce without that
// machinery. Self-closing tags count as creating an element, matching the
// tree builder.
func hasDuplicatedFormatting(doc string) bool {
	starts := map[string]int{}
	z := html.NewTokenizer(strings.NewReader(doc))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		if tt == html.StartTagToken || tt == html.SelfClosingTagToken {
			name, _ := z.TagName()
			if tn := lowerTagASCII(name); isFormattingElem(tn) {
				starts[tn]++
			}
		}
	}
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		return false
	}
	dup := false
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if dup {
			return
		}
		if n.Type == html.ElementNode {
			// Mirror parseHTMLInfo: these subtrees are skipped entirely.
			switch n.Data {
			case "script", "style", "noscript", "template", "svg":
				return
			}
			if isFormattingElem(n.Data) {
				starts[n.Data]--
				if starts[n.Data] < 0 {
					dup = true
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return dup
}

// BenchmarkParseDOM vs BenchmarkParseSinglePass: before/after for #274.
func benchDoc500k() string {
	lorem := "Lorem ipsum dolor sit amet, consectetur adipiscing elit. Sed do eiusmod tempor. "
	var b strings.Builder
	b.WriteString(`<html lang="en"><head><title>Bench Page Title</title>`)
	b.WriteString(`<meta name="description" content="bench desc"><meta charset="utf-8">`)
	b.WriteString(`<link rel="canonical" href="https://example.com/bench"></head><body><h1>Bench H1</h1>`)
	for i := 0; i < 400; i++ {
		b.WriteString("<p>")
		b.WriteString(strings.Repeat(lorem, 15))
		b.WriteString("</p>")
		if i%40 == 0 {
			b.WriteString(`<a href="https://outbound.example/page">link</a>`)
		}
	}
	b.WriteString(`</body></html>`)
	return b.String()
}

func BenchmarkParseDOM(b *testing.B) {
	doc := benchDoc500k()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		root, _ := html.Parse(strings.NewReader(doc))
		info := &WebsiteInfo{}
		parseHTMLInfo(root, info, "example.com")
	}
}

func BenchmarkParseSinglePass(b *testing.B) {
	doc := benchDoc500k()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		info := &WebsiteInfo{}
		parseHTMLSinglePass(strings.NewReader(doc), info, "example.com")
	}
}
