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
//   - Word count and SimHash text are accumulated per text token, mirroring
//     the per-text-node accounting of the DOM walk (a text run split across
//     two tokens counts the same as two adjacent text nodes).
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

	z := html.NewTokenizer(r)
	var skipTag string // non-empty while inside a skipped subtree
	skipDepth := 0
	// title/h1 capture: mirrors textOf's pendingSpace collapse.
	var capTag string // "title" or "h1" while capturing
	capDepth := 0
	var capBuf strings.Builder
	capPendingSpace := false
	capFlushSpace := func() {
		if capPendingSpace && capBuf.Len() > 0 {
			capBuf.WriteByte(' ')
		}
		capPendingSpace = false
	}
	flushCap := func() {
		s := cleanTitle(capBuf.String())
		if capTag == "title" {
			if info.Title == "" {
				info.Title = s
			}
		} else if info.H1 == "" {
			info.H1 = s
		}
		capTag = ""
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
			goto done
		case html.TextToken:
			if skipTag != "" || capSkip != "" {
				// Inside a skipped subtree: the DOM walk never sees this text.
				// Exception: script/style inside title/h1 is excluded from the
				// title too (textOf), but noscript/template/svg text still
				// counts toward the title — accumulate it into the capture.
				if capTag != "" && (capSkip == "noscript" || capSkip == "template" || capSkip == "svg") {
					accumCap(string(z.Text()), &capBuf, &capPendingSpace, capFlushSpace)
				}
				continue
			}
			data := string(z.Text())
			feedText(data)
			if capTag != "" {
				accumCap(data, &capBuf, &capPendingSpace, capFlushSpace)
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			rawName, _ := z.TagName()
			tn := strings.ToLower(string(rawName))
			start := tt == html.StartTagToken
			if skipTag != "" {
				if tn == skipTag && start {
					skipDepth++
				}
				continue
			}
			if capSkip != "" {
				if tn == capSkip && start {
					capSkipDepth++
				}
				continue
			}
			if capTag != "" {
				// Nested same-name start tag: the parser auto-closes the
				// element, so flush the capture like an end tag would.
				if tn == capTag && start {
					flushCap()
					// fall through: the fresh element starts a new capture
					// only if the field is still empty (it isn't now)
				} else if tn == "script" || tn == "style" {
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
			selfClosing := !start
			switch tn {
			case "title":
				if capTag != "" && capTag != "title" {
					break // a different capture is active: transparent content
				}
				if info.Title == "" && !selfClosing {
					capTag, capDepth = "title", 1
					capBuf.Reset()
					capPendingSpace = false
				}
			case "h1":
				if capTag != "" && capTag != "h1" {
					break // a different capture is active: transparent content
				}
				if info.H1 == "" && !selfClosing {
					capTag, capDepth = "h1", 1
					capBuf.Reset()
					capPendingSpace = false
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
			if capTag != "" && tn == capTag {
				capDepth--
				if capDepth == 0 {
					flushCap()
				}
			}
		}
	}
done:
	// Unclosed title/h1 at EOF: html.Parse auto-closes them, so flush.
	if capTag != "" {
		flushCap()
	}
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
