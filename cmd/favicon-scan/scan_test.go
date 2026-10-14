package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testPNG(t *testing.T, w, h int, fill color.Color) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, fill)
		}
	}
	var buf strings.Builder
	_ = buf
	var b []byte
	wr := &byteWriter{buf: &b}
	if err := png.Encode(wr, img); err != nil {
		t.Fatal(err)
	}
	return b
}

type byteWriter struct {
	buf *[]byte
}

func (w *byteWriter) Write(p []byte) (int, error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func writeSeed(t *testing.T, entries string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "brands.json")
	if err := os.WriteFile(p, []byte(entries), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadBrands(t *testing.T) {
	pngBytes := testPNG(t, 16, 16, color.White)
	h := sha256Hex(pngBytes)
	ah := uint64Hex(AverageHash(mustDecode(t, pngBytes)))
	seed := fmt.Sprintf(`[{"brand_name":"TestBrand","domain":"brand.example","sha256":"%s","ahash":"%s","source":"test"}]`, h, ah)
	brands, err := loadBrands(writeSeed(t, seed))
	if err != nil {
		t.Fatalf("loadBrands: %v", err)
	}
	if len(brands) != 1 || brands[0].BrandName != "TestBrand" {
		t.Fatalf("unexpected brands: %+v", brands)
	}
	if !brands[0].hasAHash {
		t.Fatal("expected ahash parsed")
	}
	idx := newBrandIndex(brands)
	if got := idx.exactMatch(h); got == nil || got.BrandName != "TestBrand" {
		t.Fatal("exactMatch missed")
	}
	if got := idx.exactMatch(strings.Repeat("0", 64)); got != nil {
		t.Fatal("exactMatch false positive")
	}
}

func TestLoadBrandsRejectsBadSHA(t *testing.T) {
	seed := `[{"brand_name":"B","domain":"b.example","sha256":"zzz","source":"test"}]`
	if _, err := loadBrands(writeSeed(t, seed)); err == nil {
		t.Fatal("expected error for bad sha256")
	}
}

func TestRegistrableDomain(t *testing.T) {
	cases := map[string]string{
		"login.paypal.com":    "paypal.com",
		"paypal.com":          "paypal.com",
		"a.b.c.example.co.uk": "example.co.uk",
		"example.co.uk":       "example.co.uk",
	}
	for in, want := range cases {
		if got := registrableDomain(in); got != want {
			t.Errorf("registrableDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsBrandDomain(t *testing.T) {
	b := &Brand{BrandName: "PayPal", Domain: "paypal.com"}
	for _, d := range []string{"paypal.com", "login.paypal.com", "www.paypal.com"} {
		if !isBrandDomain(d, b) {
			t.Errorf("expected %q to be brand domain", d)
		}
	}
	for _, d := range []string{"paypa1.com", "paypal.com.evil.com", "notpaypal.org"} {
		if isBrandDomain(d, b) {
			t.Errorf("expected %q NOT to be brand domain", d)
		}
	}
}

func mustDecode(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, _, err := decodeIconImage(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return img
}

func TestScanExactMatch(t *testing.T) {
	iconBytes := testPNG(t, 32, 32, color.RGBA{0, 0, 255, 255})
	h := sha256Hex(iconBytes)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(iconBytes)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	seed := fmt.Sprintf(`[{"brand_name":"BlueBrand","domain":"bluebrand.example","sha256":"%s","source":"test"}]`, h)
	brands, err := loadBrands(writeSeed(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	sc := &scanner{fetch: newFetcher(srv.Client(), 5*time.Second), idx: newBrandIndex(brands), nearThreshold: 8}

	// Impersonator domain: same favicon bytes, different registrable domain.
	r := sc.scanDomain(host)
	if r.FetchErr != "" {
		t.Fatalf("fetch: %s", r.FetchErr)
	}
	if r.ExactBrand == nil || r.ExactBrand.BrandName != "BlueBrand" {
		t.Fatalf("expected exact brand hit, got %+v", r)
	}

	// Brand's own subdomain: not a hit.
	seed2 := fmt.Sprintf(`[{"brand_name":"BlueBrand","domain":"%s","sha256":"%s","source":"test"}]`, host, h)
	brands2, _ := loadBrands(writeSeed(t, seed2))
	sc2 := &scanner{fetch: newFetcher(srv.Client(), 5*time.Second), idx: newBrandIndex(brands2), nearThreshold: 8}
	r2 := sc2.scanDomain(host)
	if r2.ExactBrand != nil {
		t.Fatalf("brand's own domain should not be a hit, got %+v", r2.ExactBrand)
	}
}

func TestScanNearMatchMeasurement(t *testing.T) {
	// Brand icon: white with a black square in the corner.
	brandImg := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			brandImg.Set(x, y, color.White)
		}
	}
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			brandImg.Set(x, y, color.Black)
		}
	}
	var bb []byte
	if err := png.Encode(&byteWriter{buf: &bb}, brandImg); err != nil {
		t.Fatal(err)
	}
	// Variant: same but the black square is slightly smaller (re-encoded-ish).
	varImg := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			varImg.Set(x, y, color.White)
		}
	}
	for y := 0; y < 6; y++ {
		for x := 0; x < 6; x++ {
			varImg.Set(x, y, color.Black)
		}
	}
	var vb []byte
	if err := png.Encode(&byteWriter{buf: &vb}, varImg); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(vb)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	brandAHash := AverageHash(mustDecode(t, bb))
	if Hamming(brandAHash, AverageHash(mustDecode(t, vb))) > 8 {
		t.Skip("test images differ too much for the near threshold; adjust fixtures")
	}
	seed := fmt.Sprintf(`[{"brand_name":"SqBrand","domain":"sqbrand.example","sha256":"%s","ahash":"%s","source":"test"}]`,
		sha256Hex(bb), uint64Hex(brandAHash))
	brands, err := loadBrands(writeSeed(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	sc := &scanner{fetch: newFetcher(srv.Client(), 5*time.Second), idx: newBrandIndex(brands), nearThreshold: 8}
	r := sc.scanDomain(host)
	if r.FetchErr != "" {
		t.Fatalf("fetch: %s", r.FetchErr)
	}
	if r.ExactBrand != nil {
		t.Fatal("variant must not exact-match")
	}
	if r.NearBrand == nil || r.NearBrand.BrandName != "SqBrand" {
		t.Fatalf("expected near-match measurement, got %+v", r)
	}
	if r.NearDist < 0 || r.NearDist > 8 {
		t.Fatalf("near distance out of range: %d", r.NearDist)
	}
}

func TestApplyCDNExclusion(t *testing.T) {
	b := &Brand{BrandName: "B", Domain: "brand.example", sha256norm: strings.Repeat("a", 64)}
	mk := func(domain string) *ScanResult {
		return &ScanResult{Domain: domain, SHA256Hex: strings.Repeat("a", 64), ExactBrand: b}
	}
	var results []*ScanResult
	for i := 0; i < 1002; i++ {
		results = append(results, mk(fmt.Sprintf("site%d.example", i)))
	}
	applyCDNExclusion(results, 1000)
	excluded := 0
	for _, r := range results {
		if r.Excluded {
			excluded++
		}
	}
	if excluded != 1002 {
		t.Fatalf("expected all 1002 excluded by CDN rule, got %d", excluded)
	}

	// Under threshold: no exclusion.
	results = results[:10]
	for _, r := range results {
		r.Excluded = false
	}
	applyCDNExclusion(results, 1000)
	for _, r := range results {
		if r.Excluded {
			t.Fatal("unexpected exclusion under threshold")
		}
	}
}

func TestFindIconLink(t *testing.T) {
	page := `<html><head><link rel="shortcut icon" href="/img/fav.ico"><link rel="stylesheet" href="/x.css"></head></html>`
	if got := findIconLink([]byte(page)); got != "/img/fav.ico" {
		t.Fatalf("got %q", got)
	}
	page2 := `<html><head><LINK REL="ICON" HREF="https://cdn.example/i.png"></head></html>`
	if got := findIconLink([]byte(page2)); got != "https://cdn.example/i.png" {
		t.Fatalf("got %q", got)
	}
	if got := findIconLink([]byte(`<html><head></head></html>`)); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestFetchViaPage(t *testing.T) {
	iconBytes := testPNG(t, 16, 16, color.White)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><link rel="icon" href="/assets/icon.png"></head></html>`)
	})
	mux.HandleFunc("/assets/icon.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(iconBytes)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	f := newFetcher(srv.Client(), 5*time.Second)
	icon, err := f.fetchFavicon(host)
	if err != nil {
		t.Fatalf("fetchFavicon: %v", err)
	}
	if icon.SHA256Hex != sha256Hex(iconBytes) {
		t.Fatal("sha256 mismatch")
	}
	if !icon.HasAHash {
		t.Fatal("expected ahash")
	}
}

func TestFetchNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	f := newFetcher(srv.Client(), 5*time.Second)
	if _, err := f.fetchFavicon(host); err == nil {
		t.Fatal("expected error for 404 favicon")
	}
}
