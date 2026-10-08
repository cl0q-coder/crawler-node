package trust

import (
	"context"
	"fmt"
	"math/rand"
	"time"
)

// Submission is one crawl result a node handed to the ingest path, as the
// verifier sees it: the node's claimed content hash plus the integrity
// signature from SignResult.
type Submission struct {
	NodeID      string
	URL         string
	ClaimedHash [32]byte // SHA-256 of the body the node says it fetched
	ClaimedAt   time.Time
	Signature   []byte // from SignResult, may be nil for legacy nodes
}

// Fetcher is the trusted fetch path: re-download a URL from infrastructure
// the verifier controls (not via the node under test).
type Fetcher func(ctx context.Context, url string) ([]byte, error)

// Outcome is one verification verdict.
type Outcome struct {
	NodeID  string
	URL     string
	Match   bool // trusted re-fetch agreed with the node's claimed hash
	SigOK   bool // integrity signature valid (false when no key configured)
	Detail  string
	Checked time.Time
}

// Verifier samples node submissions and re-crawls them from a trusted path.
type Verifier struct {
	// Fetch re-downloads the URL from trusted infrastructure.
	Fetch Fetcher
	// Scorer receives every outcome. Required.
	Scorer *TrustScorer
	// KeyFor returns the node secret for signature checks, or nil when the
	// node has no key on file (legacy) — signature checking is skipped then.
	KeyFor func(nodeID string) []byte
	// SampleRate in [0,1]: fraction of submissions to actually verify.
	// 0 verifies nothing, 1 verifies everything.
	SampleRate float64
	// MaxAge bounds how old a submission may be before the verifier refuses
	// it as stale (replay protection on top of the signature timestamp).
	MaxAge time.Duration

	rng *rand.Rand
}

// NewVerifier builds a Verifier. sampleRate must be in [0,1]; maxAge <= 0
// disables the staleness check.
func NewVerifier(fetch Fetcher, scorer *TrustScorer, keyFor func(nodeID string) []byte, sampleRate float64, maxAge time.Duration) *Verifier {
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

// MaybeVerify decides by sampling whether to verify sub, and if so runs the
// full check: signature, staleness, trusted re-fetch, hash comparison. The
// outcome is always recorded in the scorer. sampled=false means the
// submission was skipped by sampling (no outcome recorded).
func (v *Verifier) MaybeVerify(ctx context.Context, sub Submission) (out Outcome, sampled bool, err error) {
	if v.rng.Float64() >= v.SampleRate {
		return Outcome{}, false, nil
	}
	out = v.verify(ctx, sub)
	v.Scorer.RecordVerification(sub.NodeID, out.Match)
	return out, true, nil
}

// VerifyOne runs the full check unconditionally (sampling bypassed) — used
// by tests and by the operator re-promotion flow.
func (v *Verifier) VerifyOne(ctx context.Context, sub Submission) Outcome {
	out := v.verify(ctx, sub)
	v.Scorer.RecordVerification(sub.NodeID, out.Match)
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
	// node's own and hasn't been tampered with in transit.
	if v.KeyFor != nil {
		secret := v.KeyFor(sub.NodeID)
		if len(secret) > 0 && len(sub.Signature) > 0 {
			out.SigOK = VerifyResult(secret, sub.Signature, sub.NodeID, sub.URL, sub.ClaimedHash, sub.ClaimedAt)
			if !out.SigOK {
				out.Detail = "integrity signature invalid: submission tampered or misattributed"
				return out
			}
		}
	}

	// 3. Trusted re-fetch and hash comparison: the core honesty check.
	body, err := v.Fetch(ctx, sub.URL)
	if err != nil {
		out.Detail = fmt.Sprintf("trusted re-fetch failed: %v", err)
		return out
	}
	if ContentHash(body) != sub.ClaimedHash {
		out.Detail = "content mismatch: trusted re-fetch hash != claimed hash"
		return out
	}
	out.Match = true
	out.Detail = "match: trusted re-fetch agrees with submission"
	return out
}
