package main

// T1: Sitemap-driven discovery.
//
// Every serious crawler treats sitemaps as first-class. During the robots.txt
// pass we already fetch per host; here we also collect Sitemap: directives
// (plus a /sitemap.xml fallback), fetch them, and feed discovered URLs into
// the same outDomains discovery path the page parser uses. lastmod is parsed
// and retained for future scheduling priority (T3); today it is recorded but
// the frontier is still uniform-sampled.

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"cl0q/crawlutil"
)

// sitemapMaxBytes caps a single sitemap fetch. Sitemaps are XML; 5MB covers a
// ~50k-URL sitemap comfortably while bounding a hostile server.
const sitemapMaxBytes = 5 << 20

// sitemapMaxFiles caps how many sitemap files we follow from a sitemapindex.
const sitemapMaxFiles = 10

// sitemapEntry is one URL discovered from a sitemap.
type sitemapEntry struct {
	loc     string
	lastmod string // raw lastmod value; empty if absent
}

// sitemapURLsFromRobots extracts Sitemap: directive URLs from a robots.txt body.
// Directives are always at line start (RFC 9309); matching is case-insensitive.
func sitemapURLsFromRobots(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 9 {
			continue
		}
		if strings.HasPrefix(strings.ToLower(line), "sitemap:") {
			u := strings.TrimSpace(line[8:])
			if u != "" {
				out = append(out, u)
			}
		}
	}
	return out
}

// sitemapURLSet is the XML shape of a <urlset>.
type sitemapURLSet struct {
	URLs []struct {
		Loc     string `xml:"loc"`
		LastMod string `xml:"lastmod"`
	} `xml:"url"`
}

// sitemapIndex is the XML shape of a <sitemapindex>.
type sitemapIndex struct {
	Sitemaps []struct {
		Loc string `xml:"loc"`
	} `xml:"sitemap"`
}

// parseSitemap parses one sitemap document. It returns the URL entries; if the
// document is a sitemapindex, it returns the child sitemap URLs with lastmod
// empty and index=true so the caller can recurse.
func parseSitemap(data []byte) (entries []sitemapEntry, index []string, err error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	// Peek the root element name.
	var root struct {
		XMLName xml.Name
	}
	if err := dec.Decode(&root); err != nil {
		return nil, nil, fmt.Errorf("sitemap root: %w", err)
	}
	switch root.XMLName.Local {
	case "sitemapindex":
		var idx sitemapIndex
		if err := xml.Unmarshal(data, &idx); err != nil {
			return nil, nil, fmt.Errorf("sitemapindex: %w", err)
		}
		for _, s := range idx.Sitemaps {
			if s.Loc != "" {
				index = append(index, strings.TrimSpace(s.Loc))
			}
		}
		return nil, index, nil
	case "urlset":
		var set sitemapURLSet
		if err := xml.Unmarshal(data, &set); err != nil {
			return nil, nil, fmt.Errorf("urlset: %w", err)
		}
		for _, u := range set.URLs {
			loc := strings.TrimSpace(u.Loc)
			if loc != "" {
				entries = append(entries, sitemapEntry{loc: loc, lastmod: strings.TrimSpace(u.LastMod)})
			}
		}
		return entries, nil, nil
	default:
		return nil, nil, fmt.Errorf("unknown sitemap root %q", root.XMLName.Local)
	}
}

// fetchSitemapGET fetches one sitemap URL with caps. It reuses the caller's
// http client (which carries the netguard-guarded transport).
func fetchSitemapGET(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	setBrowserHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("sitemap status %d", resp.StatusCode)
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if ct != "" && !strings.Contains(ct, "xml") && !strings.Contains(ct, "text") && !strings.Contains(ct, "octet-stream") {
		return nil, fmt.Errorf("sitemap not XML (Content-Type %q)", ct)
	}
	return io.ReadAll(io.LimitReader(resp.Body, sitemapMaxBytes))
}

// discoverFromSitemaps fetches and parses the sitemaps for a host and returns
// discovered entries. sitemapURLs come from robots.txt Sitemap: directives;
// if empty, /sitemap.xml on the same host is tried as a fallback. All fetches
// are paced through the host/IP throttles by the caller.
func discoverFromSitemaps(ctx context.Context, client *http.Client, scheme, domain string, sitemapURLs []string) []sitemapEntry {
	urls := sitemapURLs
	if len(urls) == 0 {
		urls = []string{scheme + domain + "/sitemap.xml"}
	}
	var out []sitemapEntry
	fetched := 0
	for _, u := range urls {
		if fetched >= sitemapMaxFiles {
			break
		}
		// Only follow sitemaps on the same registrable host — a hostile
		// robots.txt could otherwise point us at arbitrary URLs.
		if hostKey(urlHost(u)) != hostKey(domain) {
			continue
		}
		data, err := fetchSitemapGET(ctx, client, u)
		if err != nil {
			continue
		}
		fetched++
		entries, index, err := parseSitemap(data)
		if err != nil {
			continue
		}
		out = append(out, entries...)
		// Recurse into sitemapindex children (same-host only, capped).
		for _, child := range index {
			if fetched >= sitemapMaxFiles {
				break
			}
			if hostKey(urlHost(child)) != hostKey(domain) {
				continue
			}
			cdata, err := fetchSitemapGET(ctx, client, child)
			if err != nil {
				continue
			}
			fetched++
			centries, _, err := parseSitemap(cdata)
			if err != nil {
				continue
			}
			out = append(out, centries...)
		}
	}
	return out
}

// urlHost extracts the hostname from a URL string; empty on parse failure.
func urlHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "://") {
		return "" // malformed: scheme separator with no scheme
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+3:]
	}
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.LastIndex(raw, "@"); i >= 0 {
		raw = raw[i+1:]
	}
	if i := strings.Index(raw, ":"); i >= 0 {
		raw = raw[:i]
	}
	return strings.ToLower(raw)
}

// sitemapDiscover runs T1 sitemap discovery for a crawled domain: if we haven't
// checked its sitemaps within sitemapRecheckInterval, fetch them (paced like any
// other request to the host) and merge discovered domains into info.outDomains
// so they flow through the normal discovery emit path.
func sitemapDiscover(domain, ip string, ri robotsInfo, info *WebsiteInfo) {
	if info == nil {
		return
	}
	now := time.Now().Unix()
	if v, ok := sitemapChecked.Load(domain); ok {
		if now-v.(int64) < int64(sitemapRecheckInterval/time.Second) {
			return
		}
	}
	sitemapChecked.Store(domain, now)

	// Pace sitemap fetches like any other request to this host/IP.
	hostThrottle(domain)
	if ip != "" {
		ipThrottle(ip)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	entries := discoverFromSitemaps(ctx, newClient(), "https://", domain, ri.sitemaps)
	if len(entries) == 0 {
		return
	}
	if info.outDomains == nil {
		info.outDomains = map[string]bool{}
	}
	added := 0
	for _, e := range entries {
		h := urlHost(e.loc)
		if h == "" || h == domain {
			continue // same-host URLs aren't discovery
		}
		if !crawlSubdomains.Load() {
			h = crawlutil.Registrable(h) // apex-only: fold to registrable
		}
		if h != "" && len(h) <= 253 && crawlutil.Registrable(h) != "" {
			if !info.outDomains[h] {
				info.outDomains[h] = true
				added++
			}
		}
	}
	if mSitemapURLs != nil {
		mSitemapURLs.Add(int64(added))
	}
}

// mSitemapURLs counts domains discovered via sitemaps (T1). Set in main()
// alongside the other counters; nil in tests that don't call it.
var mSitemapURLs *atomic.Int64
