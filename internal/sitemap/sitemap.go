// Package sitemap parses XML sitemaps (IG3). Sitemaps are the cheapest
// crawl-budget win: they list URLs explicitly and lastmod drives recrawl
// prioritization without fetching.
//
// Supports:
//   - urlset (list of URLs with optional lastmod)
//   - sitemapindex (list of sitemap URLs, recursive)
package sitemap

import (
	"encoding/xml"
	"io"
	"time"
)

// URL is a single entry in a sitemap.
type URL struct {
	Loc        string
	LastMod    time.Time
	HasLastMod bool
}

// urlset is the XML structure of a sitemap URL list.
type urlset struct {
	XMLName xml.Name   `xml:"urlset"`
	URLs    []urlEntry `xml:"url"`
}

type urlEntry struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod"`
}

// sitemapindex is the XML structure of a sitemap index.
type sitemapindex struct {
	XMLName  xml.Name       `xml:"sitemapindex"`
	Sitemaps []sitemapEntry `xml:"sitemap"`
}

type sitemapEntry struct {
	Loc string `xml:"loc"`
}

// Parse reads a sitemap from r. It returns the URLs (for urlset) or the
// sitemap URLs (for sitemapindex, with IsIndex=true).
func Parse(r io.Reader) (urls []URL, sitemapURLs []string, isIndex bool, err error) {
	data, err := io.ReadAll(io.LimitReader(r, 10<<20)) // 10MB cap
	if err != nil {
		return nil, nil, false, err
	}

	// Try sitemapindex first (it has a distinct root element).
	var idx sitemapindex
	if err := xml.Unmarshal(data, &idx); err == nil && len(idx.Sitemaps) > 0 {
		for _, s := range idx.Sitemaps {
			if s.Loc != "" {
				sitemapURLs = append(sitemapURLs, s.Loc)
			}
		}
		return nil, sitemapURLs, true, nil
	}

	// Try urlset.
	var us urlset
	if err := xml.Unmarshal(data, &us); err != nil {
		return nil, nil, false, err
	}
	for _, u := range us.URLs {
		if u.Loc == "" {
			continue
		}
		entry := URL{Loc: u.Loc}
		if u.LastMod != "" {
			if t, err := parseTime(u.LastMod); err == nil {
				entry.LastMod = t
				entry.HasLastMod = true
			}
		}
		urls = append(urls, entry)
	}
	return urls, nil, false, nil
}

// parseTime handles the various date formats in sitemaps (W3C Datetime).
func parseTime(s string) (time.Time, error) {
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05-07:00",
		"2006-01-02",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, &time.ParseError{}
}

// LatestLastMod returns the most recent lastmod from a list of URLs.
// Returns zero time if none have lastmod.
func LatestLastMod(urls []URL) time.Time {
	var latest time.Time
	for _, u := range urls {
		if u.HasLastMod && u.LastMod.After(latest) {
			latest = u.LastMod
		}
	}
	return latest
}
