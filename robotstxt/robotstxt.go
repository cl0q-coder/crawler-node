// Package robotstxt parses robots.txt the way both cl0q crawlers obey it (RFC 9309):
//   - if any group names a cl0q bot (botid.IsOurs: cl0q-node or any other cl0q bot name), those groups apply —
//     even when they contain no rules — otherwise the "*" groups do;
//   - within the applicable groups the longest matching Allow/Disallow path wins, and Allow wins a tie; "*" matches any
//     run of characters and a trailing "$" anchors the end; an empty Disallow allows everything;
//   - Crawl-delay is honoured, capped at MaxCrawlDelay so a hostile robots.txt can't stall a worker for hours.
package robotstxt

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cl0q/botid"
)

// MaxCrawlDelay caps the Crawl-delay a site can ask for.
const MaxCrawlDelay = 30 * time.Second

// MaxBytes is how much of a robots.txt file callers should read (RFC 9309 asks crawlers to parse at least 500 KiB).
const MaxBytes = 512 << 10

// Rules is the parsed robots.txt verdict for cl0q bots. A nil *Rules allows everything.
type Rules struct {
	rules []rule
	delay time.Duration
}

type rule struct {
	allow bool
	plen  int
	re    *regexp.Regexp
}

// Parse reads a robots.txt body.
func Parse(body string) *Rules {
	// RFC 9309 §2.1: strip a single leading UTF-8 BOM. Without this, a
	// BOM-prefixed first line parses as field "\ufeffuser-agent", the group
	// is never created, and AllowsPath wrongly returns true for a file that
	// disallows everything.
	body = strings.TrimPrefix(body, "\ufeff")
	type group struct {
		ours, star bool
		rules      []rule
		delay      time.Duration
	}
	var groups []*group
	var cur *group
	prevField := ""
	for _, line := range strings.Split(body, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		idx := strings.IndexByte(line, ':')
		if line == "" || idx < 0 {
			continue
		}
		field := strings.ToLower(strings.TrimSpace(line[:idx]))
		val := strings.TrimSpace(line[idx+1:])
		switch field {
		case "user-agent":
			if cur == nil || prevField != "user-agent" {
				cur = &group{} // consecutive User-agent lines share one group
				groups = append(groups, cur)
			}
			if val == "*" {
				cur.star = true
			} else if botid.IsOurs(val) {
				cur.ours = true
			}
		case "allow", "disallow":
			if cur != nil && val != "" {
				cur.rules = append(cur.rules, rule{allow: field == "allow", plen: len(val), re: pattern(val)})
			}
		case "crawl-delay":
			if cur != nil {
				if f, err := strconv.ParseFloat(val, 64); err == nil && f > 0 {
					cur.delay = time.Duration(min(f, MaxCrawlDelay.Seconds()) * float64(time.Second))
				}
			}
		}
		switch field {
		case "user-agent", "allow", "disallow", "crawl-delay":
			prevField = field // Sitemap:, Host: and other lines belong to no group and don't split one (RFC 9309)
		}
	}
	ours := false
	for _, g := range groups {
		ours = ours || g.ours
	}
	out := &Rules{}
	for _, g := range groups {
		if (ours && g.ours) || (!ours && g.star) {
			out.rules = append(out.rules, g.rules...)
			out.delay = max(out.delay, g.delay)
		}
	}
	return out
}

// pattern turns a robots path pattern (with * and a trailing $) into an anchored regexp.
func pattern(p string) *regexp.Regexp {
	end := strings.HasSuffix(p, "$")
	p = strings.TrimSuffix(p, "$")
	q := strings.ReplaceAll(regexp.QuoteMeta(escapePattern(p)), `\*`, `.*`)
	if end {
		q += "$"
	}
	re, err := regexp.Compile("^" + q)
	if err != nil {
		return regexp.MustCompile(`^\x00never`)
	}
	return re
}

// escapePattern canonicalizes a robots path pattern the way Google's reference
// parser does (RFC 9309: matching happens against the percent-encoded path,
// and Allows() feeds patterns u.EscapedPath()): UTF-8 multibyte sequences are
// percent-encoded, and existing %-escapes are normalized to uppercase hex.
// Without this, a rule like "Disallow: /café" could never match, because the
// path under test arrives as "/caf%C3%A9".
func escapePattern(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '%' && i+2 < len(p) && isHex(p[i+1]) && isHex(p[i+2]) {
			b.WriteByte('%')
			b.WriteByte(upperHex(p[i+1]))
			b.WriteByte(upperHex(p[i+2]))
			i += 2
			continue
		}
		if c >= 0x80 {
			const hexd = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hexd[c>>4])
			b.WriteByte(hexd[c&0xf])
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func upperHex(c byte) byte {
	if 'a' <= c && c <= 'f' {
		return c - ('a' - 'A')
	}
	return c
}

// Allows reports whether u's path (and query) may be fetched.
func (r *Rules) Allows(u *url.URL) bool {
	p := u.EscapedPath()
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return r.AllowsPath(p)
}

// AllowsPath reports whether a path such as "/" or "/a/b?x=1" may be fetched.
func (r *Rules) AllowsPath(p string) bool {
	if r == nil {
		return true
	}
	if p == "" {
		p = "/"
	}
	best, allow := -1, true
	for _, rl := range r.rules {
		if rl.plen >= best && rl.re.MatchString(p) && (rl.plen > best || rl.allow) {
			best, allow = rl.plen, rl.allow
		}
	}
	return allow
}

// Delay is the Crawl-delay the site asked for (0 if none), already capped at MaxCrawlDelay.
func (r *Rules) Delay() time.Duration {
	if r == nil {
		return 0
	}
	return r.delay
}
