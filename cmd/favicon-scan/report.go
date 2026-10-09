package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
)

// Hit is one reportable brand-impersonation finding.
type Hit struct {
	Domain      string `json:"domain"`
	Brand       string `json:"brand"`
	SHA256      string `json:"sha256"`
	AHashHex    string `json:"ahash,omitempty"`
	HammingDist *int   `json:"hamming_dist,omitempty"`
	IsExact     bool   `json:"is_exact"`
	IconURL     string `json:"icon_url"`
}

// Report aggregates a scan run, including the v1 re-encoded-variant measurement.
type Report struct {
	DomainsScanned int
	FetchOK        int
	FetchFailed    int
	ExactHits      int // exact SHA256 matches, CDN-excluded removed
	ExcludedCDN    int // exact matches demoted by the CDN exclusion
	NearHits       int // non-exact aHash near-matches (probable re-encoded variants)
	Hits           []Hit
	PerBrand       map[string]*brandStats
}

type brandStats struct {
	Exact    int
	Near     int // probable re-encoded variants
	Excluded int
}

func buildReport(results []*ScanResult, brands []*Brand) *Report {
	rep := &Report{PerBrand: map[string]*brandStats{}}
	for _, b := range brands {
		rep.PerBrand[b.BrandName] = &brandStats{}
	}
	for _, r := range results {
		rep.DomainsScanned++
		if r.FetchErr != "" {
			rep.FetchFailed++
			continue
		}
		rep.FetchOK++
		if r.ExactBrand != nil {
			st := rep.PerBrand[r.ExactBrand.BrandName]
			if r.Excluded {
				rep.ExcludedCDN++
				st.Excluded++
				continue
			}
			rep.ExactHits++
			st.Exact++
			var hd *int
			ah := ""
			if r.HasAHash {
				ah = uint64Hex(r.AHash)
			}
			rep.Hits = append(rep.Hits, Hit{
				Domain: r.Domain, Brand: r.ExactBrand.BrandName,
				SHA256: r.SHA256Hex, AHashHex: ah, HammingDist: hd,
				IsExact: true, IconURL: r.IconURL,
			})
		} else if r.NearBrand != nil {
			// Near-matches are measurement only in v1 (not persisted as hits).
			rep.NearHits++
			rep.PerBrand[r.NearBrand.BrandName].Near++
		}
	}
	sort.Slice(rep.Hits, func(i, j int) bool {
		if rep.Hits[i].Brand != rep.Hits[j].Brand {
			return rep.Hits[i].Brand < rep.Hits[j].Brand
		}
		return rep.Hits[i].Domain < rep.Hits[j].Domain
	})
	return rep
}

func printSummary(rep *Report) {
	log.Printf("scan complete: %d domains, %d favicons fetched, %d fetch failures",
		rep.DomainsScanned, rep.FetchOK, rep.FetchFailed)
	log.Printf("exact-match impersonators: %d (%d excluded as CDN/generic icons)",
		rep.ExactHits, rep.ExcludedCDN)
	log.Printf("probable re-encoded variants (aHash near-matches, non-exact): %d", rep.NearHits)
	if rep.ExactHits+rep.NearHits > 0 {
		rate := float64(rep.NearHits) / float64(rep.ExactHits+rep.NearHits) * 100
		log.Printf("V1 MEASUREMENT: %.1f%% of brand-favicon matches are re-encoded variants that exact-match misses", rate)
		log.Printf("  -> if this rate is high, pHash v2 is worth the backfill; if low, exact-match suffices")
	}
	names := make([]string, 0, len(rep.PerBrand))
	for n, st := range rep.PerBrand {
		if st.Exact+st.Near+st.Excluded > 0 {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		st := rep.PerBrand[n]
		log.Printf("  brand %-12s exact=%d near=%d excluded=%d", n, st.Exact, st.Near, st.Excluded)
	}
	if len(names) == 0 {
		fmt.Println("no brand-impersonation hits in this run")
	}
}

func writeJSONL(path string, hits []Hit) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, h := range hits {
		if err := enc.Encode(h); err != nil {
			return err
		}
	}
	return nil
}
