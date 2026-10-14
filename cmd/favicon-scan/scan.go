package main

// ScanResult is the per-domain outcome of one favicon scan.
type ScanResult struct {
	Domain     string
	IconURL    string
	SHA256Hex  string
	AHash      uint64
	HasAHash   bool
	ImgW       int
	ImgH       int
	Format     string
	FetchErr   string
	ExactBrand *Brand // exact SHA256 match, not the brand's own domain
	NearBrand  *Brand // best aHash near-match (non-exact), for the v1 measurement
	NearDist   int
	Excluded   bool // CDN/generic-icon exclusion applied in the second pass
}

type scanner struct {
	fetch         *fetcher
	idx           *brandIndex
	nearThreshold int
}

func (s *scanner) scanDomain(domain string) *ScanResult {
	r := &ScanResult{Domain: domain, NearDist: -1}
	icon, err := s.fetch.fetchFavicon(domain)
	if err != nil {
		r.FetchErr = err.Error()
		return r
	}
	r.IconURL = icon.IconURL
	r.SHA256Hex = icon.SHA256Hex
	r.AHash = icon.AHash
	r.HasAHash = icon.HasAHash
	r.ImgW, r.ImgH = icon.Width, icon.Height
	r.Format = icon.Format

	// Tier 1 (v1 ship): exact SHA256 match, excluding the brand's own domains.
	if b := s.idx.exactMatch(icon.SHA256Hex); b != nil && !isBrandDomain(domain, b) {
		r.ExactBrand = b
	}

	// v1 measurement: best aHash near-match that is NOT an exact match.
	// Counts probable re-encoded/resized variants of brand favicons.
	if icon.HasAHash {
		for _, b := range s.idx.all {
			if !b.hasAHash {
				continue
			}
			if b.sha256norm == iconSHA256Norm(icon.SHA256Hex) {
				continue // exact match handled above; near-match is for variants
			}
			if isBrandDomain(domain, b) {
				continue
			}
			d := Hamming(icon.AHash, b.ahash)
			if d <= s.nearThreshold && (r.NearBrand == nil || d < r.NearDist) {
				r.NearBrand = b
				r.NearDist = d
			}
		}
	}
	return r
}

func iconSHA256Norm(hexStr string) string {
	// brands.go normalizes to lowercase; keep the comparison consistent.
	norm := make([]byte, len(hexStr))
	for i := 0; i < len(hexStr); i++ {
		c := hexStr[i]
		if c >= 'A' && c <= 'F' {
			c += 'a' - 'A'
		}
		norm[i] = c
	}
	return string(norm)
}

// applyCDNExclusion demotes exact hits whose favicon is used by more than
// threshold distinct non-brand domains (generic/CDN icons, parked pages).
// Brands are never excluded by popularity (Micah's nit): only non-brand
// domains count toward the threshold.
func applyCDNExclusion(results []*ScanResult, threshold int) {
	counts := map[string]map[string]bool{} // sha256 -> set of non-brand domains
	brandOf := map[string]*Brand{}
	for _, r := range results {
		if r.ExactBrand == nil || r.SHA256Hex == "" {
			continue
		}
		set, ok := counts[r.SHA256Hex]
		if !ok {
			set = map[string]bool{}
			counts[r.SHA256Hex] = set
			brandOf[r.SHA256Hex] = r.ExactBrand
		}
		set[r.Domain] = true
	}
	for sha, set := range counts {
		b := brandOf[sha]
		nonBrand := 0
		for d := range set {
			if !isBrandDomain(d, b) {
				nonBrand++
			}
		}
		if nonBrand > threshold {
			for _, r := range results {
				if r.ExactBrand != nil && r.SHA256Hex == sha {
					r.Excluded = true
				}
			}
		}
	}
}
