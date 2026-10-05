package sitemap

import (
	"strings"
	"testing"
	"time"
)

// IG3: sitemap parser must handle urlset, sitemapindex, and lastmod dates.

const urlsetXML = `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://example.com/</loc><lastmod>2026-10-01</lastmod></url>
  <url><loc>https://example.com/about</loc><lastmod>2026-09-15T10:30:00+00:00</lastmod></url>
  <url><loc>https://example.com/contact</loc></url>
</urlset>`

const indexXML = `<?xml version="1.0" encoding="UTF-8"?>
<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <sitemap><loc>https://example.com/sitemap1.xml</loc></sitemap>
  <sitemap><loc>https://example.com/sitemap2.xml</loc></sitemap>
</sitemapindex>`

func TestParseURLSet(t *testing.T) {
	urls, sitemapURLs, isIndex, err := Parse(strings.NewReader(urlsetXML))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if isIndex {
		t.Error("urlset should not be detected as index")
	}
	if len(sitemapURLs) != 0 {
		t.Errorf("urlset should have no sitemap URLs, got %d", len(sitemapURLs))
	}
	if len(urls) != 3 {
		t.Fatalf("want 3 URLs, got %d", len(urls))
	}
	if urls[0].Loc != "https://example.com/" {
		t.Errorf("URL[0].Loc = %q", urls[0].Loc)
	}
	if !urls[0].HasLastMod {
		t.Error("URL[0] should have lastmod")
	}
	if urls[2].HasLastMod {
		t.Error("URL[2] should not have lastmod")
	}
}

func TestParseIndex(t *testing.T) {
	_, sitemapURLs, isIndex, err := Parse(strings.NewReader(indexXML))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if !isIndex {
		t.Error("sitemapindex should be detected as index")
	}
	if len(sitemapURLs) != 2 {
		t.Fatalf("want 2 sitemap URLs, got %d", len(sitemapURLs))
	}
	if sitemapURLs[0] != "https://example.com/sitemap1.xml" {
		t.Errorf("sitemap[0] = %q", sitemapURLs[0])
	}
}

func TestLatestLastMod(t *testing.T) {
	urls, _, _, _ := Parse(strings.NewReader(urlsetXML))
	latest := LatestLastMod(urls)
	want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if !latest.Equal(want) {
		t.Errorf("LatestLastMod = %v, want %v", latest, want)
	}
}

func TestLatestLastModNone(t *testing.T) {
	urls := []URL{{Loc: "https://example.com/"}}
	if latest := LatestLastMod(urls); !latest.IsZero() {
		t.Errorf("no lastmod: want zero time, got %v", latest)
	}
}

func TestParseInvalid(t *testing.T) {
	_, _, _, err := Parse(strings.NewReader("not xml at all <><"))
	if err == nil {
		t.Error("invalid XML should error")
	}
}
