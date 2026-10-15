package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
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

// maxIconPixelDim caps decoded favicon dimensions (Q4 #283 bomb guard).
// 4096x4096 NRGBA = 64 MiB worst case: far above any real favicon
// (apple-touch-icon tops out at 180x180; large PNG icons are 512x512),
// far below OOM/panic territory in image.NewNRGBA (which panics, fatally,
// on huge dimensions instead of returning an error).
const maxIconPixelDim = 4096

// iconDimsTooBig reports whether data positively identifies itself as a
// PNG, GIF, or JPEG whose declared dimensions exceed maxIconPixelDim.
// Unknown/unparseable headers return false and let image.Decode decide.
func iconDimsTooBig(data []byte) bool {
	if w, h, ok := pngDims(data); ok {
		return w > maxIconPixelDim || h > maxIconPixelDim
	}
	if w, h, ok := gifDims(data); ok {
		return w > maxIconPixelDim || h > maxIconPixelDim
	}
	if w, h, ok := jpegDims(data); ok {
		return w > maxIconPixelDim || h > maxIconPixelDim
	}
	return false
}

// pngDims reads PNG IHDR dimensions. ok=false unless the IHDR is present
// and well-formed.
func pngDims(data []byte) (w, h int, ok bool) {
	if len(data) < 24 || !bytes.HasPrefix(data, pngMagic) {
		return 0, 0, false
	}
	if binary.BigEndian.Uint32(data[8:12]) != 13 || string(data[12:16]) != "IHDR" {
		return 0, 0, false
	}
	w = int(binary.BigEndian.Uint32(data[16:20]))
	h = int(binary.BigEndian.Uint32(data[20:24]))
	return w, h, true
}

// gifDims reads GIF logical-screen dimensions. ok=false unless the header
// is present.
func gifDims(data []byte) (w, h int, ok bool) {
	if len(data) < 10 {
		return 0, 0, false
	}
	if m := string(data[:6]); m != "GIF87a" && m != "GIF89a" {
		return 0, 0, false
	}
	w = int(binary.LittleEndian.Uint16(data[6:8]))
	h = int(binary.LittleEndian.Uint16(data[8:10]))
	return w, h, true
}

// jpegDims scans JPEG markers for the first SOF segment and returns its
// dimensions. ok=false when no SOF is found or the header walk hits
// anything malformed — the caller then lets image.Decode decide.
func jpegDims(data []byte) (w, h int, ok bool) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 0, 0, false
	}
	off := 2
	for off+4 <= len(data) {
		if data[off] != 0xFF {
			return 0, 0, false
		}
		marker := data[off+1]
		off += 2
		// Standalone markers carry no length field.
		if marker == 0xD8 || marker == 0xD9 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			continue
		}
		if off+2 > len(data) {
			return 0, 0, false
		}
		segLen := int(binary.BigEndian.Uint16(data[off : off+2]))
		if segLen < 2 || off+segLen > len(data) {
			return 0, 0, false
		}
		// SOF0-SOF15 except the non-image markers C4/C8/CC.
		if marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC {
			if segLen < 7 {
				return 0, 0, false
			}
			h = int(binary.BigEndian.Uint16(data[off+3 : off+5]))
			w = int(binary.BigEndian.Uint16(data[off+5 : off+7]))
			return w, h, true
		}
		off += segLen
	}
	return 0, 0, false
}

// decodeIconImage decodes PNG/JPEG/GIF via stdlib, plus ICO (PNG-in-ICO or
// 32/24-bit BMP entries) via the minimal decoder in ico.go.
func decodeIconImage(data []byte) (image.Image, string, error) {
	// Q4 (#283) bomb guard: reject absurd declared dimensions BEFORE the
	// stdlib decoders allocate pixel buffers — huge dims panic fatally
	// inside image.NewNRGBA (or OOM the process), they don't error.
	if iconDimsTooBig(data) {
		return nil, "", fmt.Errorf("icon dimensions exceed %dpx cap", maxIconPixelDim)
	}
	if img, format, err := image.Decode(bytes.NewReader(data)); err == nil {
		return img, format, nil
	}
	img, err := decodeICO(data)
	if err != nil {
		return nil, "", fmt.Errorf("not a decodable image: %w", err)
	}
	return img, "ico", nil
}
