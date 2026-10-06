// Package crawlutil holds the helpers both crawlers share: which registrable site a host belongs to, whether an
// https failure means "this server doesn't offer TLS", and the operator's subdomain setting.
package crawlutil

import (
	"database/sql"
	"log"
	"net"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// normalize lower-cases host and strips a trailing dot and a leading "www.".
func normalize(host string) string {
	return strings.TrimPrefix(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), "."), "www.")
}

// icannSuffix returns the ICANN public suffix of h ("co.ke" for nation.co.ke, "com" for foo.blogspot.com). Private
// suffixes such as blogspot.com or github.io are skipped on purpose: their subdomains are separate blogs and pages
// on one platform, and folding them to the platform keeps one platform from flooding discovery.
func icannSuffix(h string) string {
	s, icann := publicsuffix.PublicSuffix(h)
	for !icann {
		i := strings.IndexByte(s, '.')
		if i < 0 {
			break // unlisted TLD: PublicSuffix already returned the last label
		}
		s, icann = publicsuffix.PublicSuffix(s[i+1:])
	}
	return s
}

// ipLiteralChars reports whether h consists solely of characters that can
// appear in an IP literal (hex digits, dots, colons). Typical DNS names
// contain other letters, so this lets callers skip the expensive net.ParseIP
// for the common case; anything that looks IP-ish still gets the real parse,
// so semantics are unchanged.
func ipLiteralChars(h string) bool {
	for i := 0; i < len(h); i++ {
		c := h[i]
		if c != '.' && c != ':' &&
			!(c >= '0' && c <= '9') &&
			!(c >= 'a' && c <= 'f') &&
			!(c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// Registrable returns the registrable site a host belongs to: blog.acme.com → acme.com, a.bbc.co.uk → bbc.co.uk,
// nation.co.ke → nation.co.ke, foo.blogspot.com → blogspot.com. It returns "" for a host that is itself a public
// suffix (co.ke, com), an IP address or a single label, none of which is a site.
func Registrable(host string) string {
	h := normalize(host)
	if h == "" || !strings.Contains(h, ".") {
		return ""
	}
	// net.ParseIP is the hottest allocation in the crawl path (it is tried
	// for every outbound host); skip it unless h even looks like an IP.
	if ipLiteralChars(h) && net.ParseIP(h) != nil {
		return ""
	}
	s := icannSuffix(h)
	if s == "" || h == s || !strings.HasSuffix(h, "."+s) {
		return ""
	}
	rest := strings.TrimSuffix(h, "."+s)
	if i := strings.LastIndexByte(rest, '.'); i >= 0 {
		rest = rest[i+1:]
	}
	// Degenerate inputs (".", "..", "a..com", trailing/empty labels) must collapse
	// to "" (non-registrable), not to a malformed "." — otherwise the function is
	// not idempotent: Registrable(Registrable(x)) != Registrable(x).
	if rest == "" {
		return ""
	}
	return rest + "." + s
}

// IsSubdomain reports whether host (after dropping "www.") is below its registrable site.
func IsSubdomain(host string) bool {
	r := Registrable(host)
	return r != "" && r != normalize(host)
}

// IsPublicSuffix reports whether host is itself an ICANN public suffix (co.ke, com.ng, com), which is never a site.
func IsPublicSuffix(host string) bool {
	h := normalize(host)
	return h != "" && icannSuffix(h) == h
}

// HTTPSUnavailable reports whether an https attempt failed because the server doesn't offer (valid) TLS: refused on
// :443, not a TLS endpoint, or a certificate we won't accept. Then one plain-http attempt is fair. Anything else (EOF,
// reset, timeout, closed connection) means the server is refusing or overloaded: don't knock again.
func HTTPSUnavailable(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, m := range []string{"connection refused", "no route to host", "tls:", "x509", "certificate",
		"first record does not look like a tls handshake", "server gave http response to https client"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// LoadCrawlSubdomains reads the operator's cl0q_settings.crawl_subdomains switch. A missing row means off (apex
// only); a database error keeps current, so a blip can't flip the whole crawler to apex-only.
func LoadCrawlSubdomains(db *sql.DB, current bool) bool {
	var v string
	err := db.QueryRow(`SELECT value FROM cl0q_settings WHERE key='crawl_subdomains'`).Scan(&v)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		log.Printf("crawl_subdomains load: %v — keeping current setting (%v)", err, current)
		return current
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "on", "yes":
		return true
	}
	return false
}
