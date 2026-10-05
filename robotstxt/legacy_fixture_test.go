package robotstxt

import (
	"regexp"
	"strings"
)

// legacyPattern is the PRE-D3 regex-based pattern compiler, kept as a
// TEST-ONLY fixture for differential testing (Claude's D3 condition 3).
// It must never be used in production code — the regexp.Compile-per-rule
// cost is the tarpit D3 eliminates.
//
// It turns a robots path pattern (with * and a trailing $) into an anchored regexp.
func legacyPattern(p string) *regexp.Regexp {
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

// legacyMatch reports whether path matches pattern using the legacy regex engine.
func legacyMatch(pattern, path string) bool {
	return legacyPattern(pattern).MatchString(path)
}
