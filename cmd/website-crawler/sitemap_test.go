package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestSitemapURLsFromRobots(t *testing.T) {
	body := `User-agent: *
Disallow: /private/
Sitemap: https://example.com/sitemap.xml
SITEMAP: https://example.com/sitemap-news.xml
  Sitemap:   https://example.com/sitemap2.xml  
# Sitemap: https://example.com/commented.xml
Host: example.com`
	got := sitemapURLsFromRobots(body)
	if len(got) != 3 {
		t.Fatalf("got %d sitemap URLs %v, want 3", len(got), got)
	}
	if got[0] != "https://example.com/sitemap.xml" {
		t.Errorf("got[0] = %q", got[0])
	}
	if got[1] != "https://example.com/sitemap-news.xml" {
		t.Errorf("got[1] = %q (case-insensitive directive)", got[1])
	}
	if got[2] != "https://example.com/sitemap2.xml" {
		t.Errorf("got[2] = %q (whitespace)", got[2])
	}
	if len(sitemapURLsFromRobots("User-agent: *\nDisallow: /")) != 0 {
		t.Error("expected no sitemap URLs")
	}
}

func TestParseSitemapURLSet(t *testing.T) {
	data := `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://example.com/</loc><lastmod>2026-10-01</lastmod></url>
  <url><loc>https://example.com/about</loc></url>
  <url><loc>  </loc><lastmod>2026-10-02</lastmod></url>
</urlset>`
	entries, index, err := parseSitemap([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(index) != 0 {
		t.Errorf("unexpected index entries: %v", index)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (empty loc skipped)", len(entries))
	}
	if entries[0].loc != "https://example.com/" || entries[0].lastmod != "2026-10-01" {
		t.Errorf("entries[0] = %+v", entries[0])
	}
	if entries[1].lastmod != "" {
		t.Errorf("entries[1].lastmod = %q, want empty", entries[1].lastmod)
	}
}

func TestParseSitemapIndex(t *testing.T) {
	data := `<?xml version="1.0" encoding="UTF-8"?>
<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <sitemap><loc>https://example.com/s1.xml</loc></sitemap>
  <sitemap><loc>https://example.com/s2.xml</loc></sitemap>
</sitemapindex>`
	entries, index, err := parseSitemap([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("unexpected url entries: %v", entries)
	}
	if len(index) != 2 || index[0] != "https://example.com/s1.xml" {
		t.Errorf("index = %v", index)
	}
}

func TestParseSitemapBad(t *testing.T) {
	for _, tc := range []string{
		``,
		`not xml at all`,
		`<html><body>nope</body></html>`,
		`<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"></feed>`,
	} {
		if _, _, err := parseSitemap([]byte(tc)); err == nil {
			t.Errorf("expected error for %q", tc)
		}
	}
}

func TestURLHost(t *testing.T) {
	cases := map[string]string{
		"https://example.com/path?q=1":   "example.com",
		"http://sub.example.com:8080/x":  "sub.example.com",
		"https://user:pass@example.com/": "example.com",
		"example.com/no-scheme":          "example.com",
		"":                               "",
		"://broken":                      "",
	}
	for in, want := range cases {
		if got := urlHost(in); got != want {
			t.Errorf("urlHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func BenchmarkParseSitemap50k(b *testing.B) {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for i := 0; i < 50000; i++ {
		fmt.Fprintf(&sb, `<url><loc>https://example.com/page-%d</loc><lastmod>2026-10-01</lastmod></url>`, i)
	}
	sb.WriteString(`</urlset>`)
	data := []byte(sb.String())
	b.Logf("sitemap size: %d bytes", len(data))
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		entries, _, err := parseSitemap(data)
		if err != nil {
			b.Fatal(err)
		}
		if len(entries) != 50000 {
			b.Fatalf("got %d entries", len(entries))
		}
	}
}
