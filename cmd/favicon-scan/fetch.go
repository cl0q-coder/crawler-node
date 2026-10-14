package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const (
	maxIconBytes = 2 << 20 // 2MB per favicon
	maxPageBytes = 5 << 20 // 5MB homepage cap for <link rel=icon> discovery
)

// fetchedIcon is one downloaded favicon and its fingerprints.
type fetchedIcon struct {
	IconURL   string
	Bytes     []byte
	SHA256Hex string
	AHash     uint64
	HasAHash  bool
	Width     int
	Height    int
	Format    string
}

type fetcher struct {
	client  *http.Client
	timeout time.Duration
	ua      string
}

func newFetcher(client *http.Client, timeout time.Duration) *fetcher {
	return &fetcher{client: client, timeout: timeout, ua: "cl0q-favicon-scan/1.0 (+https://cl0q.com)"}
}

// fetchFavicon finds and downloads the favicon for domain:
// 1. GET https://domain/ (http fallback), parse <link rel="icon"|"shortcut icon"|"apple-touch-icon">
// 2. fall back to https://domain/favicon.ico (http fallback)
func (f *fetcher) fetchFavicon(domain string) (*fetchedIcon, error) {
	for _, scheme := range []string{"https", "http"} {
		icon, pageErr := f.fetchViaPage(scheme, domain)
		if icon != nil {
			return icon, nil
		}
		// /favicon.ico direct attempt on the same scheme before switching schemes.
		icoURL := scheme + "://" + domain + "/favicon.ico"
		if icon, err := f.getIcon(icoURL); err == nil {
			return icon, nil
		}
		if pageErr != nil && scheme == "http" {
			return nil, fmt.Errorf("page fetch: %v; favicon.ico also failed", pageErr)
		}
	}
	return nil, fmt.Errorf("no favicon found for %s", domain)
}

func (f *fetcher) fetchViaPage(scheme, domain string) (*fetchedIcon, error) {
	pageURL := scheme + "://" + domain + "/"
	body, finalURL, err := f.get(pageURL, maxPageBytes)
	if err != nil {
		return nil, err
	}
	href := findIconLink(body)
	if href == "" {
		return nil, fmt.Errorf("no <link rel=icon> on %s", pageURL)
	}
	u, err := url.Parse(href)
	if err != nil {
		return nil, fmt.Errorf("bad icon href %q: %w", href, err)
	}
	base, _ := url.Parse(finalURL)
	iconURL := base.ResolveReference(u).String()
	return f.getIcon(iconURL)
}

func (f *fetcher) getIcon(iconURL string) (*fetchedIcon, error) {
	body, finalURL, err := f.get(iconURL, maxIconBytes)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	img, format, err := decodeIconImage(body)
	fi := &fetchedIcon{
		IconURL:   finalURL,
		Bytes:     body,
		SHA256Hex: hex.EncodeToString(sum[:]),
		Format:    format,
	}
	if err == nil && img != nil {
		b := img.Bounds()
		fi.Width, fi.Height = b.Dx(), b.Dy()
		fi.AHash = AverageHash(img)
		fi.HasAHash = true
	}
	return fi, nil
}

func (f *fetcher) get(rawURL string, maxBytes int64) (body []byte, finalURL string, err error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", f.ua)
	req.Header.Set("Accept", "image/*,*/*;q=0.8")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("GET %s: status %d", rawURL, resp.StatusCode)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) > maxBytes {
		return nil, "", fmt.Errorf("GET %s: body exceeds %d bytes", rawURL, maxBytes)
	}
	u := resp.Request.URL.String()
	return body, u, nil
}

// findIconLink extracts the first <link rel="icon"|"shortcut icon"|"apple-touch-icon"> href.
func findIconLink(page []byte) string {
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return ""
	}
	var href string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if href != "" {
			return
		}
		if n.Type == html.ElementNode && n.Data == "link" {
			var rel, h string
			for _, a := range n.Attr {
				switch strings.ToLower(a.Key) {
				case "rel":
					rel = strings.ToLower(a.Val)
				case "href":
					h = strings.TrimSpace(a.Val)
				}
			}
			if h != "" && (rel == "icon" || rel == "shortcut icon" || rel == "apple-touch-icon" || rel == "apple-touch-icon-precomposed") {
				href = h
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return href
}

// decodeIconImage decodes PNG/JPEG/GIF via stdlib, plus ICO (PNG-in-ICO or
// 32/24-bit BMP entries) via the minimal decoder in ico.go.
func decodeIconImage(data []byte) (image.Image, string, error) {
	if img, format, err := image.Decode(bytes.NewReader(data)); err == nil {
		return img, format, nil
	}
	img, err := decodeICO(data)
	if err != nil {
		return nil, "", fmt.Errorf("not a decodable image: %w", err)
	}
	return img, "ico", nil
}
