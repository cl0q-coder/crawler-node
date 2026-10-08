package trust

import (
	"context"
	"testing"
	"time"
)

func testSecret(nodeID string) []byte { return []byte("secret-for-" + nodeID) }

func submit(nodeID, url string, body []byte, secret []byte, at time.Time) Submission {
	h := ContentHash(body)
	return Submission{
		NodeID:      nodeID,
		URL:         url,
		ClaimedHash: h,
		ClaimedAt:   at,
		Signature:   SignResult(secret, nodeID, url, h, at),
	}
}

// trustedFetcher serves the ground truth the verifier trusts.
func trustedFetcher(pages map[string][]byte) Fetcher {
	return func(ctx context.Context, url string) ([]byte, error) {
		b, ok := pages[url]
		if !ok {
			return nil, context.DeadlineExceeded
		}
		return b, nil
	}
}

func TestSignVerifyRoundtrip(t *testing.T) {
	secret := []byte("s3cret")
	h := ContentHash([]byte("<html>hi</html>"))
	ts := time.Now().Truncate(time.Second)
	sig := SignResult(secret, "node-1", "https://example.com/", h, ts)
	if !VerifyResult(secret, sig, "node-1", "https://example.com/", h, ts) {
		t.Fatal("valid signature rejected")
	}
	// Tamper with each field: all must fail.
	tampered := ContentHash([]byte("<html>EVIL</html>"))
	if VerifyResult(secret, sig, "node-1", "https://example.com/", tampered, ts) {
		t.Fatal("tampered content hash accepted")
	}
	if VerifyResult(secret, sig, "node-1", "https://example.com/other", h, ts) {
		t.Fatal("tampered URL accepted")
	}
	if VerifyResult(secret, sig, "node-2", "https://example.com/", h, ts) {
		t.Fatal("tampered nodeID accepted")
	}
	if VerifyResult(secret, sig, "node-1", "https://example.com/", h, ts.Add(time.Second)) {
		t.Fatal("tampered timestamp accepted")
	}
	if VerifyResult([]byte("wrong"), sig, "node-1", "https://example.com/", h, ts) {
		t.Fatal("wrong secret accepted")
	}
	if !VerifyResult(secret, append([]byte{}, sig...), "node-1", "https://example.com/", h, ts) {
		t.Fatal("copied signature rejected")
	}
}

func TestPoWRounds(t *testing.T) {
	for _, diff := range []uint{8, 12, 16} {
		ch := Challenge{Seed: "test-seed-abc", Difficulty: diff}
		nonce, err := SolveChallenge(ch, "node-9")
		if err != nil {
			t.Fatalf("difficulty %d: %v", diff, err)
		}
		if !VerifySolution(ch, "node-9", nonce) {
			t.Fatalf("difficulty %d: solution not verified", diff)
		}
		// A wrong nonce must (overwhelmingly) fail. Even if it passed by
		// freak chance, the solver's own nonce verifies — the point here is
		// the verifier is deterministic on the same input.
		if VerifySolution(Challenge{Seed: "other-seed", Difficulty: diff}, "node-9", nonce) {
			t.Fatal("solution verified under a different seed")
		}
	}
	if _, err := SolveChallenge(Challenge{Seed: "s", Difficulty: 0}, "n"); err == nil {
		t.Fatal("difficulty 0 accepted")
	}
	if _, err := SolveChallenge(Challenge{Seed: "s", Difficulty: 33}, "n"); err == nil {
		t.Fatal("difficulty 33 accepted")
	}
	if _, err := SolveChallenge(Challenge{Seed: "", Difficulty: 8}, "n"); err == nil {
		t.Fatal("empty seed accepted")
	}
}

// TestDishonestNodeDetection is the G-12 deliverable demo: an honest node
// and a dishonest node (submits tampered content hashes) run against the
// verifier. The honest node climbs out of provisional to TierFull; the
// dishonest node is detected and quarantined; RouteWork cuts it off.
func TestDishonestNodeDetection(t *testing.T) {
	ctx := context.Background()
	groundTruth := map[string][]byte{
		"https://example.com/a": []byte("<html><h1>real A</h1></html>"),
		"https://example.com/b": []byte("<html><h1>real B</h1></html>"),
	}
	scorer := NewTrustScorer()
	v := NewVerifier(trustedFetcher(groundTruth), scorer, testSecret, 1.0, time.Hour)

	honest, evil := "honest-node", "evil-node"
	now := time.Now().Truncate(time.Second)

	for i := 0; i < 60; i++ {
		url := "https://example.com/a"
		if i%2 == 1 {
			url = "https://example.com/b"
		}
		// Honest node: signs what it really fetched.
		out := v.VerifyOne(ctx, submit(honest, url, groundTruth[url], testSecret(honest), now))
		if !out.Match {
			t.Fatalf("honest submission failed verification: %s", out.Detail)
		}
		// Dishonest node: claims a DIFFERENT hash than the trusted
		// re-fetch produces (poisoned/spam content), but signs it properly
		// — the signature is valid, the content is a lie. This is exactly
		// the case signature-only checking misses and re-crawl catches.
		evilBody := []byte("<html><h1>SPAM - buy now</h1></html>")
		evilSub := submit(evil, url, evilBody, testSecret(evil), now)
		out = v.VerifyOne(ctx, evilSub)
		if out.Match {
			t.Fatal("dishonest submission passed verification")
		}
		if !out.SigOK {
			t.Fatal("dishonest node's signature should be VALID (it signed its own lie)")
		}
	}

	if got := scorer.Tier(honest); got != TierFull {
		t.Fatalf("honest node tier = %v, want full (score=%.3f)", got, scorer.Score(honest))
	}
	if got := scorer.Tier(evil); got != TierQuarantined {
		t.Fatalf("dishonest node tier = %v, want quarantined (score=%.3f)", got, scorer.Score(evil))
	}
	if RouteWork(TierFull) != "standard" || RouteWork(TierRestricted) != "low-value" || RouteWork(TierQuarantined) != "none" {
		t.Fatal("RouteWork mapping wrong")
	}
}

// TestProvisionalCap: a brand-new node with a perfect record still cannot
// reach TierFull until it has enough outcomes on record.
func TestProvisionalCap(t *testing.T) {
	scorer := NewTrustScorer()
	for i := 0; i < MinOutcomesBeforeFull-1; i++ {
		scorer.RecordVerification("new-node", true)
	}
	if got := scorer.Tier("new-node"); got == TierFull {
		t.Fatal("provisional node reached TierFull too early")
	}
	if got := scorer.Tier("new-node"); got != TierRestricted {
		t.Fatalf("provisional perfect node = %v, want restricted", got)
	}
	scorer.RecordVerification("new-node", true) // hits MinOutcomesBeforeFull
	if got := scorer.Tier("new-node"); got != TierFull {
		t.Fatalf("node with %d perfect outcomes = %v, want full", MinOutcomesBeforeFull, got)
	}
}

// TestStaleSubmissionRefused: replays of old signed submissions are refused.
func TestStaleSubmissionRefused(t *testing.T) {
	ctx := context.Background()
	pages := map[string][]byte{"https://example.com/": []byte("x")}
	scorer := NewTrustScorer()
	v := NewVerifier(trustedFetcher(pages), scorer, testSecret, 1.0, time.Minute)
	old := submit("node-1", "https://example.com/", pages["https://example.com/"], testSecret("node-1"), time.Now().Add(-time.Hour))
	out := v.VerifyOne(ctx, old)
	if out.Match {
		t.Fatal("stale submission passed")
	}
	// One stale replay shouldn't nuke a node, but a stream of them must.
	for i := 0; i < 25; i++ {
		v.VerifyOne(ctx, old)
	}
	if got := scorer.Tier("node-1"); got != TierQuarantined {
		t.Fatalf("replay-flooding node = %v, want quarantined (score=%.3f)", got, scorer.Score("node-1"))
	}
}

// TestQualityAndUptimeSignals: bad batches and missed heartbeats drag the
// score down even when verification outcomes are clean.
func TestQualityAndUptimeSignals(t *testing.T) {
	scorer := NewTrustScorer()
	for i := 0; i < MinOutcomesBeforeFull; i++ {
		scorer.RecordVerification("shaky-node", true)
		scorer.RecordHeartbeat("shaky-node", true)
	}
	if got := scorer.Tier("shaky-node"); got != TierFull {
		t.Fatalf("clean node = %v, want full", got)
	}
	scorer.RecordQuality("shaky-node", 90, 100) // 90% of rows corrupt
	for i := 0; i < 10; i++ {
		scorer.RecordHeartbeat("shaky-node", false) // flapping
	}
	if got := scorer.Tier("shaky-node"); got == TierFull {
		t.Fatalf("bad-quality node still full (score=%.3f)", scorer.Score("shaky-node"))
	}
}

// TestQuarantineHold: operator hold pins the node regardless of score.
func TestQuarantineHold(t *testing.T) {
	scorer := NewTrustScorer()
	for i := 0; i < MinOutcomesBeforeFull; i++ {
		scorer.RecordVerification("held-node", true)
	}
	scorer.SetQuarantineHold("held-node")
	if got := scorer.Tier("held-node"); got != TierQuarantined {
		t.Fatal("quarantine hold not enforced")
	}
	scorer.ClearQuarantineHold("held-node")
	if got := scorer.Tier("held-node"); got != TierFull {
		t.Fatalf("hold release did not restore tier: %v", got)
	}
}

// TestVerifierSampling: SampleRate=0 verifies nothing and records nothing.
func TestVerifierSampling(t *testing.T) {
	ctx := context.Background()
	pages := map[string][]byte{"https://example.com/": []byte("x")}
	scorer := NewTrustScorer()
	v := NewVerifier(trustedFetcher(pages), scorer, nil, 0.0, 0)
	sub := submit("node-1", "https://example.com/", pages["https://example.com/"], nil, time.Now())
	if _, sampled, _ := v.MaybeVerify(ctx, sub); sampled {
		t.Fatal("SampleRate=0 still sampled")
	}
	if scorer.Outcomes("node-1") != 0 {
		t.Fatal("unsampled submission recorded an outcome")
	}
}
