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
	q := strings.ReplaceAll(regexp.QuoteMeta(p), `\*`, `.*`)
	if end {
		q += "$"
	}
	re, err := regexp.Compile("^" + q)
	if err != nil {
		return regexp.MustCompile(`^\x00never`)
	}
	return re
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
