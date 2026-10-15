package main

// #274 phase 2, bottleneck #1 (HTML parsing ~47% of CPU, ~2/3 of allocations).
//
// parseHTMLSinglePass is a single-pass html.Tokenizer reimplementation of
// parseHTMLInfo. It extracts exactly the same fields (title, h1, meta,
// canonical, outlinks, img count, page language, word count, SimHash text)
// without building the DOM: html.Parse allocates a node per element and per
// text run (~65% of all heap objects in the #274 baseline profile), then the
// recursive walk visits every node a second time. The tokenizer visits each
// token once and allocates ~one small string per tag.
//
// Equivalence notes (verified by TestParseSinglePassDifferential):
//   - Tag dispatch lowercases with strings.ToLower, matching html.Parse's
//     lowercased n.Data. Attribute keys are compared case-insensitively,
//     exactly like readMeta and the link/anchor handling in parseHTMLInfo.
//   - Skipped subtrees (script/style/noscript/template/svg): the DOM walk
//     returns before visiting children; here a depth counter skips tokens
//     until the matching end tag. html.Tokenizer already treats
//     script/style/title as raw text, mirroring the parser's RCDATA handling,
//     so title text containing '<' behaves the same (cleanTitle cuts at '<').
//   - Word count and SimHash text are accumulated per maximal text run
//     (adjacent text tokens coalesced into one feed), mirroring the DOM walk:
//     the tree builder merges adjacent character tokens into a single text
//     node, while the tokenizer can split a run (truncated tag at EOF). Any
//     tag token or skipped text ends the run, matching DOM node boundaries.
//   - title/h1 text: textOf() concatenates descendant text with the
//     pendingSpace collapse (skipping script/style descendants only); the
//     capture below reproduces that collapse while ALSO feeding the same text
//     tokens to the word/SimHash path, because the DOM walk visits title/h1
//     text-node children after textOf ran. A nested same-name start tag
//     flushes the capture, mirroring the parser auto-closing the element.
//   - Meta handling is a line-for-line port of readMeta (last attr wins per
//     key; first meta tag wins per field).
//   - Anchor handling is a line-for-line port of the walk's case "a" (first
//     href attr only, hasHost gate, same-site check, registrable fold, 4096
//     outDomains cap).

import (
	"io"
	"net/url"
	"strings"
	"unicode"

	"golang.org/x/net/html"

	"cl0q/crawlutil"
	"cl0q/internal/simhash"
)

// isSkipTag reports the subtrees the DOM walk never descends into.
func isSkipTag(tn string) bool {
	switch tn {
	case "script", "style", "noscript", "template", "svg":
		return true
	}
	return false
}

// dropNUL matches html.Parse: the tree builder ignores U+0000 character
// tokens, while html.Tokenizer passes NUL bytes through in text tokens.
// Strip them so both paths see identical text (found by FuzzParseSinglePass).
// In foreign content (svg/math) the tree builder substitutes U+FFFD instead
// — normText handles that case.
func dropNUL(s string) string {
	if strings.IndexByte(s, 0) < 0 {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// parseHTMLSinglePass parses r (already transcoded to UTF-8) in one tokenizer
// pass, filling the same info fields parseHTMLInfo fills from the DOM.
func parseHTMLSinglePass(r io.Reader, info *WebsiteInfo, domain string) {
	base := strings.ToLower(strings.TrimPrefix(domain, "www."))
	info.outDomains = make(map[string]bool, 16)
	words := 0
	// IG2: collect text for SimHash (capped to bound memory).
	var textBuf strings.Builder
	textBuf.Grow(8192)
	feedText := func(data string) {
		words += countWords(data)
		// IG2: accumulate text for SimHash, capped at 32KB.
		if textBuf.Len() < 32768 {
			textBuf.WriteString(data)
			textBuf.WriteByte(' ')
		}
	}
	// textRun coalesces adjacent text tokens into a single feedText call.
	// The DOM walk sees one text node per maximal character run (the tree
	// builder merges adjacent character tokens), but the tokenizer can split
	// a run across two text tokens (e.g. "0</" -> "0" + "</" when a tag is
	// truncated at EOF — found by FuzzParseSinglePass). Feeding per token
	// would double-count a word straddling the split and skew the SimHash
	// text, so the run is buffered and fed whole. Any tag token or skipped
	// text ends the run, matching DOM node boundaries.
	var textRun strings.Builder
	flushTextRun := func() {
		if textRun.Len() > 0 {
			feedText(textRun.String())
			textRun.Reset()
		}
	}

	z := html.NewTokenizer(r)
	var skipTag string // non-empty while inside a skipped subtree
	skipDepth := 0
	// title/h1 capture: mirrors textOf's pendingSpace collapse. A stack of
	// frames (not a single slot) because the DOM walk treats <title> as a
	// real element even inside <h1> — h1 is not RCDATA, so <h1><title>0 sets
	// BOTH Title and H1, and the title's text counts toward the h1 too
	// (textOf includes descendants). Text feeds every open frame.
	// elemDepth is len(elemStack) just after the frame's element was
	// pushed; popElemTo drops frames whose element is gone.
	type capFrame struct {
		tag          string // "title" or "h1"
		buf          strings.Builder
		pendingSpace bool
		elemDepth    int
	}
	var capStack []*capFrame
	accumFrame := func(fr *capFrame, data string) {
		accumCap(data, &fr.buf, &fr.pendingSpace, func() {
			if fr.pendingSpace && fr.buf.Len() > 0 {
				fr.buf.WriteByte(' ')
			}
			fr.pendingSpace = false
		})
	}
	flushCapFrame := func(fr *capFrame) {
		s := cleanTitle(fr.buf.String())
		if fr.tag == "title" {
			if info.Title == "" {
				info.Title = s
			}
		} else if info.H1 == "" {
			info.H1 = s
		}
	}
	// elemStack tracks open elements (lowercased tag names) so the h1
	// start-tag rule can mirror the tree builder exactly (x/net inBodyIM:
	// pop p's in button scope, then pop a top h1-h6). It is also used to
	// keep capture frames in sync when end tags close elements. Void
	// elements are never pushed. This is an approximation of the full
	// open-elements stack (no foster parenting, no adoption agency), but
	// the h1 rule only inspects the top after p-popping, which this gets
	// right; the fuzzer guards the rest.
	var elemStack []string
	const (
		scopeDefault = iota
		scopeListItem
		scopeButton
	)
	isVoidElem := func(tn string) bool {
		switch tn {
		case "area", "base", "br", "col", "embed", "hr", "img",
			"input", "link", "meta", "source", "track", "wbr":
			return true
		}
		return false
	}
	isH1H6 := func(tn string) bool {
		return tn == "h1" || tn == "h2" || tn == "h3" ||
			tn == "h4" || tn == "h5" || tn == "h6"
	}
	// Section 12.2.4.2 stop tags (x/net defaultScopeStopTags[""]).
	isScopeStop := func(tn string) bool {
		switch tn {
		case "applet", "caption", "html", "table", "td", "th",
			"marquee", "object", "template", "select":
			return true
		}
		return false
	}
	// isScopeCloser reports whether tn is in x/net's inBodyIM end-tag group
	// (address..ul): implied end tags, then popUntil in default scope.
	isScopeCloser := func(tn string) bool {
		switch tn {
		case "address", "article", "aside", "blockquote", "button",
			"center", "details", "dialog", "dir", "div", "dl", "fieldset",
			"figcaption", "figure", "footer", "header", "hgroup", "listing",
			"main", "menu", "nav", "ol", "pre", "search", "section",
			"select", "summary", "ul":
			return true
		}
		return false
	}
	// foreignDepth counts open svg/math elements (even skipped ones): the
	// tree builder substitutes U+FFFD for NUL bytes in foreign content
	// instead of dropping them. Reset to 0 on svg breakout (which pops all
	// foreign elements up to the HTML content).
	foreignDepth := 0
	// inForeign reports whether the current position is inside foreign
	// content (an svg/math subtree), where the tree builder substitutes
	// U+FFFD for NUL bytes instead of dropping them (x/net parse.go).
	inForeign := func() bool { return foreignDepth > 0 }
	// normText normalizes a text token the way the tree builder would:
	// NUL -> U+FFFD in foreign content, NUL dropped elsewhere.
	normText := func(data string) string {
		if strings.IndexByte(data, 0) < 0 {
			return data
		}
		if inForeign() {
			return strings.ReplaceAll(data, "\x00", "�")
		}
		return dropNUL(data)
	}
	// isBreakoutTag mirrors x/net's foreign-content breakout list: these
	// start tags pop an svg element and reprocess as HTML (plus font with
	// color/face/size attributes).
	isBreakoutTag := func(tn string) bool {
		switch tn {
		case "b", "big", "blockquote", "body", "br", "center", "code",
			"dd", "div", "dl", "dt", "em", "embed", "h1", "h2", "h3",
			"h4", "h5", "h6", "head", "hr", "i", "img", "li", "listing",
			"menu", "meta", "nobr", "ol", "p", "pre", "ruby", "s",
			"small", "span", "strong", "strike", "sub", "sup", "table",
			"tt", "u", "ul", "var":
			return true
		case "font":
			more := true
			for more {
				var k []byte
				var m bool
				k, _, m = z.TagAttr()
				more = m
				if equalFoldASCII(k, "color") || equalFoldASCII(k, "face") ||
					equalFoldASCII(k, "size") {
					return true
				}
			}
		}
		return false
	}
	// popElemTo truncates elemStack to length n, dropping capture frames
	// whose element is gone. A dropped frame writes its field only if no
	// same-tag frame remains open below it (the DOM walk visits the outer
	// element first, so the outermost frame's text — which includes the
	// inner frames' text — is the one that counts).
	popElemTo := func(n int) {
		if n < 0 {
			n = 0
		}
		if n > len(elemStack) {
			n = len(elemStack)
		}
		elemStack = elemStack[:n]
		kept := capStack[:0]
		for _, fr := range capStack {
			if fr.elemDepth > n {
				inner := false
				for _, k := range kept {
					if k.tag == fr.tag {
						inner = true
						break
					}
				}
				if !inner {
					flushCapFrame(fr)
				}
				continue
			}
			kept = append(kept, fr)
		}
		for i := len(kept); i < len(capStack); i++ {
			capStack[i] = nil
		}
		capStack = kept
	}
	// popUntilScope truncates elemStack below the highest in-scope match,
	// mirroring x/net's popUntil (match beats stop tags at each level),
	// keeping capture frames in sync via popElemTo.
	popUntilScope := func(sc int, tags ...string) {
		for i := len(elemStack) - 1; i >= 0; i-- {
			t := elemStack[i]
			for _, m := range tags {
				if t == m {
					popElemTo(i)
					return
				}
			}
			if isScopeStop(t) || (sc == scopeButton && t == "button") ||
				(sc == scopeListItem && (t == "ol" || t == "ul")) {
				return
			}
		}
	}
	// genImpliedEnds pops implied-end-tag elements (x/net
	// generateImpliedEndTags) from the top of elemStack.
	genImpliedEnds := func() {
		for len(elemStack) > 0 {
			switch elemStack[len(elemStack)-1] {
			case "dd", "dt", "li", "optgroup", "option", "p",
				"rb", "rp", "rt", "rtc":
				popElemTo(len(elemStack) - 1)
			default:
				return
			}
		}
	}
	// isSpecialElem mirrors x/net's isSpecialElementMap (used by the
	// generic end-tag rule, which stops at the first special element).
	isSpecialElem := func(tn string) bool {
		switch tn {
		case "address", "applet", "area", "article", "aside", "base",
			"basefont", "bgsound", "blockquote", "body", "br", "button",
			"caption", "center", "col", "colgroup", "dd", "details", "dir",
			"div", "dl", "dt", "embed", "fieldset", "figcaption", "figure",
			"footer", "form", "frame", "frameset", "h1", "h2", "h3", "h4",
			"h5", "h6", "head", "header", "hgroup", "hr", "html", "iframe",
			"img", "input", "keygen", "li", "link", "listing", "main",
			"marquee", "menu", "meta", "nav", "noembed", "noframes",
			"noscript", "object", "ol", "p", "param", "plaintext", "pre",
			"script", "section", "select", "source", "style", "summary",
			"table", "tbody", "td", "template", "textarea", "tfoot", "th",
			"thead", "title", "tr", "track", "ul", "wbr", "xmp":
			return true
		}
		return false
	}
	// Inside a title/h1 capture, textOf skips script/style descendants only
	// (noscript/template/svg text still counts toward the title). capSkip
	// suppresses those tokens for the word/SimHash path but not the capture.
	var capSkip string
	capSkipDepth := 0

	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			flushTextRun()
			goto done
		case html.TextToken:
			if skipTag != "" || capSkip != "" {
				// Inside a skipped subtree: the DOM walk never sees this text.
				flushTextRun()
				// Exception: script/style inside title/h1 is excluded from the
				// title too (textOf), but noscript/template/svg text still
				// counts toward the title — accumulate it into the capture.
				// (NUL handling is foreign-aware via normText: svg text gets
				// U+FFFD like the tree builder produces.)
				if len(capStack) > 0 && (capSkip == "noscript" || capSkip == "template" || capSkip == "svg") {
					data := normText(string(z.Text()))
					for i := range capStack {
						accumFrame(capStack[i], data)
					}
				}
				continue
			}
			data := normText(string(z.Text()))
			textRun.WriteString(data)
			for i := range capStack {
				accumFrame(capStack[i], data)
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			rawName, _ := z.TagName()
			tn := strings.ToLower(string(rawName))
			// Document-structure tags never create text-node boundaries:
			// duplicate html/head/body starts are ignored by the tree
			// builder (attributes merged at most), and text around them is
			// relocated into the body, merging runs the tokenizer splits
			// (fuzzer-found: "0<head>0" is one DOM text node).
			if tn != "html" && tn != "head" && tn != "body" {
				flushTextRun() // element boundary ends the text run
			}
			start := tt == html.StartTagToken
			// The tree builder ignores a self-closing flag on non-void
			// elements (parse error, e.g. "<h1/>" opens h1). Mirror it for
			// element/capture purposes — but not for skip regions: an
			// opened-and-closed <svg/> has no content to skip.
			effStart := start || (tt == html.SelfClosingTagToken && !isVoidElem(tn))
			// Track foreign-content depth for NUL normalization (counted
			// even for skipped subtrees — the skip only hides tokens from
			// field extraction, not from foreign-ness).
			if start && (tn == "svg" || tn == "math") {
				foreignDepth++
			}
			// SVG foreign-content breakout: the tree builder pops an svg
			// element and reprocesses breakout tags (b, div, h1, ...) as
			// HTML, so their content counts (fuzzer-found: "<svg><B>0" has
			// WordCount 1, but a plain subtree skip would swallow it).
			// Applies to both the subtree skip (skipTag) and the in-capture
			// skip (capSkip). (script/style are tokenizer rawtext and
			// noscript/template have no breakout.)
			if skipTag == "svg" || capSkip == "svg" {
				if tn == "svg" && start {
					if skipTag == "svg" {
						skipDepth++
					} else {
						capSkipDepth++
					}
					continue
				}
				if start && isBreakoutTag(tn) {
					if skipTag == "svg" {
						skipTag, skipDepth = "", 0
					} else {
						capSkip, capSkipDepth = "", 0
					}
					// Breakout pops all foreign elements up to HTML content.
					// (svg is never on elemStack — the skip continues before
					// the push — so there is nothing to pop there.)
					foreignDepth = 0
				} else {
					continue
				}
			} else if skipTag != "" {
				if tn == skipTag && start {
					skipDepth++
				}
				continue
			} else if capSkip != "" {
				if tn == capSkip && start {
					capSkipDepth++
				}
				continue
			}
			// h1-h6 start-tag rule, mirroring x/net's inBodyIM exactly: pop
			// p's in button scope, then pop a top h1-h6. popElemTo keeps
			// capture frames in sync (a popped h1 frame writes its field).
			if effStart && isH1H6(tn) {
				popUntilScope(scopeButton, "p")
				if len(elemStack) > 0 && isH1H6(elemStack[len(elemStack)-1]) {
					popElemTo(len(elemStack) - 1)
				}
			}
			if len(capStack) > 0 {
				if tn == "script" || tn == "style" {
					if start {
						capSkip, capSkipDepth = tn, 1
					}
					continue
				} else if isSkipTag(tn) {
					if start {
						capSkip, capSkipDepth = tn, 1
					}
					continue
				}
			} else if isSkipTag(tn) && start {
				skipTag, skipDepth = tn, 1
				continue
			}
			// Track open elements for the h1 rule above and end-tag mirroring
			// below. Void and self-closing tags never stay open; neither do
			// html/head/body — the tree builder ignores duplicate starts
			// (attributes merged at most), so they must not hide an h1 from
			// the h1 rule's top check (fuzzer-found: "<h1>0<BodY><h1>0").
			// <image> is rewritten to <img> (void) by the tree builder.
			if effStart && !isVoidElem(tn) && tn != "html" && tn != "head" &&
				tn != "body" && tn != "image" {
				elemStack = append(elemStack, tn)
			}
			switch tn {
			case "title":
				// No "transparent content" skip: a <title> inside an active
				// h1 capture is a real element per the DOM walk (h1 is not
				// RCDATA), so it starts its own frame; text feeds both.
				// (<title> inside <title> can't happen: the tokenizer treats
				// title content as RCDATA.)
				if info.Title == "" && effStart {
					capStack = append(capStack, &capFrame{tag: "title", elemDepth: len(elemStack)})
				}
			case "h1":
				if info.H1 == "" && effStart {
					capStack = append(capStack, &capFrame{tag: "h1", elemDepth: len(elemStack)})
				}
			case "meta":
				readMetaAttrs(z, info)
			case "link":
				var rel, href string
				more := true
				for more {
					var k, v []byte
					k, v, more = z.TagAttr()
					switch {
					case equalFoldASCII(k, "rel"):
						rel = string(v)
					case equalFoldASCII(k, "href"):
						href = string(v)
					}
					if rel != "" && href != "" {
						break // a valid link tag carries one rel and one href
					}
				}
				if strings.EqualFold(rel, "canonical") && info.Canonical == "" {
					info.Canonical = dropIfLong(href, maxURLLen)
				}
			case "a":
				href := ""
				more := true
				for more {
					var k, v []byte
					k, v, more = z.TagAttr()
					if equalFoldASCII(k, "href") {
						// Only the first href attribute is used (duplicates are invalid HTML).
						href = strings.TrimSpace(string(v))
						break
					}
				}
				// Port of the walk's case "a".
				if hasHost(href) {
					if u, e := url.Parse(href); e == nil && u.Host != "" {
						w := strings.TrimPrefix(u.Hostname(), "www.")
						if !strings.EqualFold(w, base) {
							host := strings.ToLower(w)
							info.OutboundLinkCount++
							if (u.Scheme == "" || u.Scheme == "http" || u.Scheme == "https") &&
								len(host) <= 253 {
								if reg := crawlutil.Registrable(host); reg != "" {
									emit := host
									if !crawlSubdomains.Load() {
										emit = reg
									}
									if len(info.outDomains) < 4096 {
										info.outDomains[emit] = true
									}
								}
							}
						}
					}
				}
			case "img":
				info.ImageCount++
			case "html":
				if info.PageLanguage == "" {
					more := true
					for more {
						var k, v []byte
						k, v, more = z.TagAttr()
						if equalFoldASCII(k, "lang") {
							info.PageLanguage = clip(string(v), maxTagLen)
							break
						}
					}
				}
			}
		case html.EndTagToken:
			rawName, _ := z.TagName()
			tn := strings.ToLower(string(rawName))
			// As with start tags, </html></head></body> never split text
			// nodes (text after them is reprocessed into the body).
			if tn != "html" && tn != "head" && tn != "body" {
				flushTextRun() // element boundary ends the text run
			}
			// Foreign-content depth tracks the tree builder even inside
			// skipped subtrees.
			if tn == "svg" || tn == "math" {
				foreignDepth--
				if foreignDepth < 0 {
					foreignDepth = 0
				}
			}
			if skipTag != "" {
				if tn == skipTag {
					skipDepth--
					if skipDepth == 0 {
						skipTag = ""
					}
				}
				continue
			}
			if capSkip != "" {
				if tn == capSkip {
					capSkipDepth--
					if capSkipDepth == 0 {
						capSkip = ""
					}
				}
				continue
			}
			// End-tag handling mirrors x/net's inBodyIM on elemStack; popElemTo
			// keeps capture frames in sync (outermost-writes).
			switch {
			case tn == "br":
				// x/net treats </br> as <br>: no stack change.
			case tn == "p":
				popUntilScope(scopeButton, "p")
			case isH1H6(tn):
				// No implied end tags, no scope pre-check (x/net popUntil).
				popUntilScope(scopeDefault, "h1", "h2", "h3", "h4", "h5", "h6")
			case tn == "li":
				popUntilScope(scopeListItem, "li")
			case tn == "dd" || tn == "dt":
				popUntilScope(scopeDefault, tn)
			case isScopeCloser(tn):
				// address..ul case (x/net inBodyIM): implied end tags,
				// then popUntil in default scope.
				genImpliedEnds()
				popUntilScope(scopeDefault, tn)
			default:
				// inBodyEndTagOther approximation: walk down from the top;
				// stop at the first special element (formatting-element
				// adoption-agency cases are out of scope — see the fuzzer
				// skip list).
				for i := len(elemStack) - 1; i >= 0; i-- {
					if elemStack[i] == tn {
						popElemTo(i)
						break
					}
					if isSpecialElem(elemStack[i]) {
						break
					}
				}
			}
		default:
			// Comment/doctype tokens: in the DOM they are separate siblings
			// between text nodes, so they end the text run.
			flushTextRun()
		}
	}
done:
	flushTextRun() // EOF ends the final text run
	// Unclosed title/h1 at EOF: html.Parse auto-closes them, so flush —
	// outermost first, mirroring the DOM walk's pre-order visit (a later
	// same-tag frame's write is suppressed by the info-field guard in
	// flushCapFrame once the outer frame has written).
	for i := range capStack {
		flushCapFrame(capStack[i])
	}
	capStack = nil
	info.WordCount = words // body text itself is never stored — only this count
	// IG2: SimHash fingerprint for near-duplicate detection.
	if textBuf.Len() > 0 {
		info.SimHash = simhash.Fingerprint(textBuf.String())
	}
}

// accumCap appends data to a title/h1 capture with textOf's pendingSpace
// whitespace collapse.
func accumCap(data string, buf *strings.Builder, pending *bool, flush func()) {
	for _, r := range data {
		if unicode.IsSpace(r) {
			*pending = true
		} else {
			flush()
			buf.WriteRune(r)
		}
	}
}

// readMetaAttrs is readMeta ported to tokenizer attributes: last attr wins per
// key; the first meta tag wins per info field.
func readMetaAttrs(z *html.Tokenizer, info *WebsiteInfo) {
	var name, prop, content, charset string
	more := true
	for more {
		var k, v []byte
		k, v, more = z.TagAttr()
		switch {
		case equalFoldASCII(k, "name"):
			name = strings.ToLower(string(v))
		case equalFoldASCII(k, "property"):
			prop = strings.ToLower(string(v))
		case equalFoldASCII(k, "content"):
			content = string(v)
		case equalFoldASCII(k, "charset"):
			charset = string(v)
		}
	}
	set := func(dst *string, v string) {
		if *dst == "" && v != "" {
			*dst = v
		}
	}
	if charset != "" {
		set(&info.Charset, clip(charset, maxTagLen))
	}
	switch name {
	case "description":
		set(&info.Description, clip(content, maxDescLen))
	case "keywords":
		set(&info.Keywords, clip(content, maxKeywordsLen))
	case "robots":
		set(&info.Robots, clip(content, maxShortLen))
	case "twitter:card":
		set(&info.TwitterCard, clip(content, maxTagLen))
	}
	switch prop {
	case "og:description":
		set(&info.Description, clip(content, maxDescLen))
	case "og:title":
		set(&info.OGTitle, cleanTitle(content))
	case "og:image":
		set(&info.OGImage, dropIfLong(content, maxURLLen))
	}
}

// equalFoldASCII reports whether k equals s under ASCII case folding, without
// allocating. Attribute keys in the wild are ASCII.
func equalFoldASCII(k []byte, s string) bool {
	if len(k) != len(s) {
		return false
	}
	for i := 0; i < len(k); i++ {
		c, d := k[i], s[i]
		if c == d {
			continue
		}
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if 'A' <= d && d <= 'Z' {
			d += 'a' - 'A'
		}
		if c != d {
			return false
		}
	}
	return true
}
