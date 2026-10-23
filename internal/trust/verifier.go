package trust

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"codeberg.org/cl0qsearch/crawler-node/internal/simhash"
)

// VerifySimHashThreshold is the max Hamming distance between the trusted
// re-fetch's SimHash fingerprint and the node's claimed fingerprint for a
// MATCH. It is deliberately looser than simhash.DuplicateThreshold (3):
// dedup must not drop distinct pages (false positives are costly), while
// verification must not penalize honest nodes for the live web's dynamic
// fragments (false negatives are costly). Measured on short dynamic pages:
// honest drift (rotated ad/CSRF tokens, whitespace) = 5 bits;
// wholesale content replacement = 25 bits. 12 separates both with margin,
// and the margin only widens on longer pages. A poisoner making *subtle*
// edits is indistinguishable from honest drift by any fuzzy hash — that
// case is bounded by continued sampling, not by this threshold.
const VerifySimHashThreshold = 12

// Submission is one crawl result a node handed to the ingest path, as the
// verifier sees it: the node's claimed content fingerprints plus the
// integrity signature from SignResult.
type Submission struct {
	NodeID         string
	URL            string
	ClaimedHash    [32]byte // SHA-256 of the body the node says it fetched (identity, signed)
	ClaimedSimHash uint64   // simhash.Fingerprint of the body (fuzzy content, signed)
	ClaimedAt      time.Time
	Signature      []byte // from SignResult, may be nil for legacy nodes
}

// Fetcher is the trusted fetch path: re-download a URL from infrastructure
// the verifier controls (not via the node under test).
type Fetcher func(ctx context.Context, url string) ([]byte, error)

// Outcome is one verification verdict.
type Outcome struct {
	NodeID       string
	URL          string
	Match        bool // trusted re-fetch agreed with the node's claimed content
	SigOK        bool // integrity signature valid (false when no key configured)
	Inconclusive bool // true when the check could not run (e.g. re-fetch failed):
	// an inconclusive outcome records NOTHING in the scorer — it is
	// neither a match nor a miss. See verify() step 3.
	Detail  string
	Checked time.Time
}

// Verifier samples node submissions and re-crawls them from a trusted path.
// Safe for concurrent use.
type Verifier struct {
	// Fetch re-downloads the URL from trusted infrastructure.
	Fetch Fetcher
	// Scorer receives every CONCLUSIVE outcome. Required.
	Scorer *TrustScorer
	// KeyFor returns the node's SIGNING key — DeriveSigningKey applied to
	// its refresh_secret, provisioned server-side at enrollment and stored
	// in the node's signing_key column (see integrity.go KEY CONTRACT).
	// The second return reports whether a key is provisioned for the node:
	// false means a legacy/pre-signing node with no key on file, for which
	// the signature check is skipped (SigOK stays false; the trusted
	// re-fetch is the only honesty check). A provisioned node (true) with
	// a missing or invalid signature is REJECTED — the check fails closed,
	// never open.
	KeyFor func(nodeID string) (key []byte, provisioned bool)
	// SampleRate in [0,1]: fraction of submissions to actually verify.
	// 0 verifies nothing, 1 verifies everything. Sampling is TIER-BLIND:
	// full-tier nodes are sampled at the same rate as restricted ones, so a
	// node cannot earn trust and then defect unobserved (see G12-TRUST.md).
	SampleRate float64
	// MaxAge bounds how old a submission may be before the verifier refuses
	// it as stale (replay protection on top of the signature timestamp).
	MaxAge time.Duration

	mu  sync.Mutex // guards rng
	rng *rand.Rand
}

// NewVerifier builds a Verifier. sampleRate must be in [0,1]; maxAge <= 0
// disables the staleness check.
func NewVerifier(fetch Fetcher, scorer *TrustScorer, keyFor func(nodeID string) ([]byte, bool), sampleRate float64, maxAge time.Duration) *Verifier {
	if scorer == nil {
		scorer = NewTrustScorer()
	}
	if sampleRate < 0 {
		sampleRate = 0
	}
	if sampleRate > 1 {
		sampleRate = 1
	}
	return &Verifier{
		Fetch:      fetch,
		Scorer:     scorer,
		KeyFor:     keyFor,
		SampleRate: sampleRate,
		MaxAge:     maxAge,
		rng:        rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (v *Verifier) drawSample() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.rng.Float64() < v.SampleRate
}

// MaybeVerify decides by sampling whether to verify sub, and if so runs the
// full check: signature, staleness, trusted re-fetch, fuzzy content
// comparison. Conclusive outcomes are recorded in the scorer; INCONCLUSIVE
// outcomes (trusted re-fetch failed) record nothing — they are neither a
// match nor a miss. sampled=false means the submission was skipped by
// sampling (no outcome recorded).
func (v *Verifier) MaybeVerify(ctx context.Context, sub Submission) (out Outcome, sampled bool, err error) {
	if !v.drawSample() {
		return Outcome{}, false, nil
	}
	out = v.verify(ctx, sub)
	if !out.Inconclusive {
		v.Scorer.RecordVerification(sub.NodeID, out.Match)
	}
	return out, true, nil
}

// VerifyOne runs the full check unconditionally (sampling bypassed) — used
// by tests and by the operator re-promotion flow. Inconclusive outcomes
// still record nothing.
func (v *Verifier) VerifyOne(ctx context.Context, sub Submission) Outcome {
	out := v.verify(ctx, sub)
	if !out.Inconclusive {
		v.Scorer.RecordVerification(sub.NodeID, out.Match)
	}
	return out
}

func (v *Verifier) verify(ctx context.Context, sub Submission) (out Outcome) {
	out = Outcome{NodeID: sub.NodeID, URL: sub.URL, Checked: time.Now()}

	// 1. Staleness: a signature is bound to a timestamp; refuse old replays.
	if v.MaxAge > 0 && time.Since(sub.ClaimedAt) > v.MaxAge {
		out.Detail = "stale submission: claimed_at outside verifier window"
		return out
	}

	// 2. Signature: the node claims this content; prove the claim is the
	// node's own and hasn't been tampered with in transit. Fail CLOSED:
	// a provisioned node with a missing or invalid signature is rejected
	// outright. Only legacy nodes (no key provisioned) skip the check —
	// and even then SigOK stays false so a skipped check is never
	// mistaken for an enforced signature.
	if v.KeyFor != nil {
		secret, provisioned := v.KeyFor(sub.NodeID)
		switch {
		case !provisioned:
			// Legacy/pre-signing node: no key on file. Signature check
			// skipped; the trusted re-fetch below is the only honesty
			// check.
		case len(secret) == 0 || len(sub.Signature) == 0:
			// Provisioned node that didn't sign (or empty key on file):
			// fail closed, do not fall through to the re-fetch.
			out.Detail = "integrity signature missing for provisioned node: rejecting submission"
			return out
		case !VerifyResult(secret, sub.Signature, sub.NodeID, sub.URL, sub.ClaimedHash, sub.ClaimedSimHash, sub.ClaimedAt):
			out.Detail = "integrity signature invalid: submission tampered or misattributed"
			return out
		default:
			out.SigOK = true
		}
	}

	// 3. Trusted re-fetch and fuzzy content comparison: the core honesty
	// check. A fetch failure is INCONCLUSIVE, not a miss: a failed
	// re-fetch is usually verifier-side (target down for us,
	// rate-limited, geo-blocked, transient) and is attacker-exploitable
	// (pick URLs that block the verifier's egress IP), so it must not
	// penalize the node. No outcome is recorded.
	body, err := v.Fetch(ctx, sub.URL)
	if err != nil {
		out.Inconclusive = true
		out.Detail = fmt.Sprintf("inconclusive: trusted re-fetch failed: %v", err)
		return out
	}

	// Fuzzy comparison via SimHash near-duplicate: the live web is dynamic
	// (CSRF/session tokens, ads, personalization, CDN/compression variance,
	// trailing whitespace), so a byte-exact SHA-256 comparison would rack
	// up mismatches against HONEST nodes and score them down unfairly.
	// Near-duplicate (Hamming distance <= VerifySimHashThreshold) means the
	// node's claimed content is genuinely what the verifier saw.
	refetchFP := simhash.Fingerprint(string(body))
	dist := simhash.Distance(refetchFP, sub.ClaimedSimHash)
	if dist > VerifySimHashThreshold {
		out.Detail = fmt.Sprintf("content mismatch: trusted re-fetch is not a near-duplicate of claimed content (simhash distance %d > %d)",
			dist, VerifySimHashThreshold)
		return out
	}
	out.Match = true
	out.Detail = "match: trusted re-fetch is a near-duplicate of claimed content"
	return out
}
