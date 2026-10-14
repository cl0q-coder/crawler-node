package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// Brand is one curated brand fingerprint from the seed file.
type Brand struct {
	BrandName string `json:"brand_name"`
	Domain    string `json:"domain"`
	SHA256    string `json:"sha256"` // hex of raw favicon bytes
	AHashHex  string `json:"ahash"`  // hex of 64-bit average hash (v1 re-encode measurement)
	Source    string `json:"source"`

	sha256norm string
	ahash      uint64
	hasAHash   bool
}

// brandIndex holds exact-match (sha256 -> brand) and near-match (slice) indexes.
type brandIndex struct {
	bySHA256 map[string]*Brand
	all      []*Brand
}

func loadBrands(path string) ([]*Brand, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var brands []*Brand
	if err := json.Unmarshal(raw, &brands); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, b := range brands {
		b.sha256norm = strings.ToLower(strings.TrimSpace(b.SHA256))
		if len(b.sha256norm) != 64 {
			return nil, fmt.Errorf("brand %q: sha256 must be 64 hex chars, got %d", b.BrandName, len(b.sha256norm))
		}
		if _, err := hex.DecodeString(b.sha256norm); err != nil {
			return nil, fmt.Errorf("brand %q: bad sha256 hex: %w", b.BrandName, err)
		}
		if b.AHashHex != "" {
			v, err := parseUint64Hex(b.AHashHex)
			if err != nil {
				return nil, fmt.Errorf("brand %q: bad ahash hex: %w", b.BrandName, err)
			}
			b.ahash = v
			b.hasAHash = true
		}
		if b.BrandName == "" || b.Domain == "" {
			return nil, fmt.Errorf("brand entry missing brand_name or domain: %+v", b)
		}
	}
	return brands, nil
}

func newBrandIndex(brands []*Brand) *brandIndex {
	idx := &brandIndex{bySHA256: make(map[string]*Brand, len(brands)), all: brands}
	for _, b := range brands {
		// First wins on duplicate sha256; seed is curated, collisions are a data bug.
		if _, ok := idx.bySHA256[b.sha256norm]; !ok {
			idx.bySHA256[b.sha256norm] = b
		}
	}
	return idx
}

func (idx *brandIndex) exactMatch(sha256hex string) *Brand {
	return idx.bySHA256[strings.ToLower(sha256hex)]
}

// registrableDomain returns the eTLD+1 for host, falling back to the last two
// labels when the publicsuffix list can't answer (IP literals, odd TLDs).
func registrableDomain(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(host, ".")
	if etld1, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return etld1
	}
	parts := strings.Split(host, ".")
	if len(parts) >= 2 {
		return strings.Join(parts[len(parts)-2:], ".")
	}
	return host
}

// isBrandDomain reports whether domain belongs to the brand itself
// (registrable-domain comparison, so login.paypal.com is the brand, not a hit).
func isBrandDomain(domain string, b *Brand) bool {
	return registrableDomain(domain) == registrableDomain(b.Domain)
}

func parseUint64Hex(s string) (uint64, error) {
	s = strings.TrimSpace(strings.TrimPrefix(s, "0x"))
	b, err := hex.DecodeString(s)
	if err != nil {
		return 0, err
	}
	if len(b) != 8 {
		return 0, fmt.Errorf("need 8 bytes, got %d", len(b))
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v, nil
}

func uint64Hex(v uint64) string {
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
	return hex.EncodeToString(b[:])
}
