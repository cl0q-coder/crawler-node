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

func TestParseSinglePassDifferential(t *testing.T) {
	lorem := "Lorem ipsum dolor sit amet, consectetur adipiscing elit. "
	bigPara := strings.Repeat("<p>"+strings.Repeat(lorem, 20)+"</p>", 200) // ~500KB
	cases := map[string]string{
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
			<meta name="description" content="big"></head><body>` + bigPara +
			`<a href="https://far.com/">x</a></body></html>`,
		"table-foster":    `<html><body><table><a href="https://foster.com/">stray</a><tr><td>cell</td></tr></table></body></html>`,
		"head-body-order": `<html><body><p>body first</p></body><head><title>Late</title></head></html>`,
		"dup-meta-charset": `<html><head><meta charset="utf-8"><meta charset="iso-8859-1">
			<meta name="description" content="x"></head><body></body></html>`,
	}
	for name, doc := range cases {
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
