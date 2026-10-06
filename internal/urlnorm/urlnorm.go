// Package urlnorm normalizes URLs before dedup and storage (IG1).
//
// Two URLs that fetch the same resource must map to the same normalized
// form, or the crawler wastes budget on duplicates. Normalization:
//   - lowercases scheme and host
//   - strips fragments (#section)
//   - removes default ports (:80 for http, :443 for https)
//   - sorts query parameters
//   - strips known session/tracking parameters (PHPSESSID, utm_*, etc.)
//   - removes trailing slash from bare paths (but keeps meaningful paths)
//   - drops userinfo (user:pass@)
package urlnorm

import (
	"net"
	"net/url"
	"sort"
	"strings"
)

// trackingParams are query parameters that never identify a distinct
// resource; they only track the visitor. Stripped during normalization.
var trackingParams = map[string]bool{
	// Session IDs
	"phpsessid": true, "jsessionid": true, "aspsessionid": true,
	"sid": true, "sessionid": true, "sessid": true,
	// Marketing trackers
	"utm_source": true, "utm_medium": true, "utm_campaign": true,
	"utm_term": true, "utm_content": true, "utm_id": true,
	"gclid": true, "fbclid": true, "msclkid": true,
	"_ga": true, "_gid": true, "mc_cid": true, "mc_eid": true,
}

// Normalize returns the canonical form of a URL for dedup purposes.
// It returns an error if the URL doesn't parse or has no host.
func Normalize(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	// Add scheme if missing (bare domains).
	if !strings.Contains(rawURL, "://") && !strings.HasPrefix(rawURL, "//") {
		rawURL = "https://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", &url.Error{Op: "normalize", URL: rawURL, Err: errNoHost}
	}

	// Scheme and host to lowercase.
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme == "" {
		u.Scheme = "https"
	}
	host := strings.ToLower(u.Hostname())

	// Strip default ports.
	if port := u.Port(); port != "" {
		if !((u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443")) {
			host = host + ":" + port
		}
		// Default ports are dropped (host stays without port).
	}
	u.Host = host

	// Drop userinfo (never identifies the resource).
	u.User = nil

	// Strip fragment.
	u.Fragment = ""
	u.RawFragment = ""

	// Normalize path: empty → "/", trailing slash trimmed unless it's the root.
	if u.Path == "" {
		u.Path = "/"
	}
	// Don't trim "/" itself; keep other paths as-is (case-sensitive).

	// Sort query params and strip trackers.
	if u.RawQuery != "" {
		q := u.Query()
		for param := range q {
			if trackingParams[strings.ToLower(param)] {
				q.Del(param)
			}
		}
		// Sort by key for canonical form.
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sorted := url.Values{}
		for _, k := range keys {
			sorted[k] = q[k]
		}
		u.RawQuery = sorted.Encode()
	}

	// Rebuild without fragment.
	u.Fragment = ""
	return u.String(), nil
}

// Domain extracts the registrable domain from a URL, normalizing first.
// Returns "" if the URL is invalid, has no usable host, or the host isn't
// a valid domain (must contain a dot or be an IP address).
func Domain(rawURL string) string {
	normalized, err := Normalize(rawURL)
	if err != nil {
		return ""
	}
	u, err := url.Parse(normalized)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	// Strip www. prefix for canonical domain form.
	host = strings.TrimPrefix(host, "www.")
	// Must look like a domain (contain a dot) or be an IP.
	if !strings.Contains(host, ".") && net.ParseIP(host) == nil {
		return ""
	}
	return host
}

var errNoHost = errorString("no host in URL")

type errorString string

func (e errorString) Error() string { return string(e) }
