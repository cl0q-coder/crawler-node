package trust

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"cl0q/internal/simhash"
)

// refreshSecret is the node's enrollment secret (what the node holds).
func refreshSecret(nodeID string) string { return "secret-for-" + nodeID }

// serverKeyFor models the server side of the KEY CONTRACT: the server
// provisioned DeriveSigningKey(refresh_secret) at enrollment and hands it
// to the verifier. The server never sees the plaintext refresh_secret for
// signature purposes.
func serverKeyFor(nodeID string) []byte { return DeriveSigningKey(refreshSecret(nodeID)) }

func submit(nodeID, url string, body []byte, secret []byte, at time.Time) Submission {
	h := ContentHash(body)
	fp := simhash.Fingerprint(string(body))
	return Submission{
		NodeID:         nodeID,
		URL:            url,
		ClaimedHash:    h,
		ClaimedSimHash: fp,
		ClaimedAt:      at,
		Signature:      SignResult(secret, nodeID, url, h, fp, at),
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
	fp := simhash.Fingerprint("<html>hi</html>")
	ts := time.Now().Truncate(time.Second)
	sig := SignResult(secret, "node-1", "https://example.com/", h, fp, ts)
	if !VerifyResult(secret, sig, "node-1", "https://example.com/", h, fp, ts) {
		t.Fatal("valid signature rejected")
	}
	// Tamper with each field: all must fail.
	tampered := ContentHash([]byte("<html>EVIL</html>"))
	if VerifyResult(secret, sig, "node-1", "https://example.com/", tampered, fp, ts) {
		t.Fatal("tampered content hash accepted")
	}
	if VerifyResult(secret, sig, "node-1", "https://example.com/", h, fp+1, ts) {
		t.Fatal("tampered simhash accepted")
	}
	if VerifyResult(secret, sig, "node-1", "https://example.com/other", h, fp, ts) {
		t.Fatal("tampered URL accepted")
	}
	if VerifyResult(secret, sig, "node-2", "https://example.com/", h, fp, ts) {
		t.Fatal("tampered nodeID accepted")
	}
	if VerifyResult(secret, sig, "node-1", "https://example.com/", h, fp, ts.Add(time.Second)) {
		t.Fatal("tampered timestamp accepted")
	}
	if VerifyResult([]byte("wrong"), sig, "node-1", "https://example.com/", h, fp, ts) {
		t.Fatal("wrong secret accepted")
	}
	if !VerifyResult(secret, append([]byte{}, sig...), "node-1", "https://example.com/", h, fp, ts) {
		t.Fatal("copied signature rejected")
	}
}

// TestDeriveSigningKeyIsStable: the node and the server must derive the
// same key independently from the refresh secret — that is the whole point
// of option (b).
func TestDeriveSigningKeyIsStable(t *testing.T) {
	a := DeriveSigningKey("refresh-secret-xyz")
	b := DeriveSigningKey("refresh-secret-xyz")
	if string(a) != string(b) {
		t.Fatal("derivation not deterministic")
	}
	if string(a) == string(DeriveSigningKey("refresh-secret-other")) {
		t.Fatal("derivation ignores the secret")
	}
	if string(a) == "refresh-secret-xyz" {
		t.Fatal("derivation returned the raw secret")
	}
}

// TestKeyForBlocker is the BLOCKER from Claude's deep review: the server
// stores only a derivation of the secret, so KeyFor must return a key the
// node's signature actually verifies under. A node-signed submission must
// VERIFY server-side; a tampered field must FAIL.
func TestKeyForBlocker(t *testing.T) {
	ctx := context.Background()
	body := []byte("<html><h1>real page</h1></html>")
	pages := map[string][]byte{"https://example.com/": body}
	scorer := NewTrustScorer()
	// The node signs with its LOCALLY derived key; the verifier gets the
	// SERVER-provisioned derived key via KeyFor. Neither side needs the
	// other's plaintext secret.
	v := NewVerifier(trustedFetcher(pages), scorer, serverKeyFor, 1.0, time.Hour)

	node, now := "node-7", time.Now().Truncate(time.Second)
	out := v.VerifyOne(ctx, submit(node, "https://example.com/", body, DeriveSigningKey(refreshSecret(node)), now))
	if !out.SigOK {
		t.Fatalf("server-side signature check failed on a node-signed submission: %s", out.Detail)
	}
	if !out.Match {
		t.Fatalf("honest node-signed submission did not match: %s", out.Detail)
	}

	// Tamper with the claimed content after signing: signature must fail.
	bad := submit(node, "https://example.com/", body, DeriveSigningKey(refreshSecret(node)), now)
	bad.ClaimedHash = ContentHash([]byte("<html>EVIL</html>"))
	out = v.VerifyOne(ctx, bad)
	if out.SigOK {
		t.Fatal("tampered submission passed the server-side signature check")
	}
	if out.Match {
		t.Fatal("tampered submission matched")
	}

	// Legacy node: KeyFor returns nil -> signature skipped, re-fetch is the
	// only check. Must not be mistaken for enforced signatures.
	v2 := NewVerifier(trustedFetcher(pages), NewTrustScorer(), nil, 1.0, time.Hour)
	out = v2.VerifyOne(ctx, submit(node, "https://example.com/", body, DeriveSigningKey(refreshSecret(node)), now))
	if out.SigOK {
		t.Fatal("SigOK true with nil KeyFor: skipped check must not report OK")
	}
	if !out.Match {
		t.Fatalf("legacy (unsigned) honest submission should still match on content: %s", out.Detail)
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
// and a dishonest node (submits tampered content) run against the
// verifier. The honest node climbs out of provisional to TierFull; the
// dishonest node is detected and quarantined; RouteWork cuts it off.
func TestDishonestNodeDetection(t *testing.T) {
	ctx := context.Background()
	groundTruth := map[string][]byte{
		"https://example.com/a": []byte("<html><h1>real A</h1><p>some body text here for fingerprinting</p></html>"),
		"https://example.com/b": []byte("<html><h1>real B</h1><p>some body text here for fingerprinting</p></html>"),
	}
	scorer := NewTrustScorer()
	v := NewVerifier(trustedFetcher(groundTruth), scorer, serverKeyFor, 1.0, time.Hour)

	honest, evil := "honest-node", "evil-node"
	now := time.Now().Truncate(time.Second)

	for i := 0; i < 60; i++ {
		url := "https://example.com/a"
		if i%2 == 1 {
			url = "https://example.com/b"
		}
		// Honest node: signs what it really fetched.
		out := v.VerifyOne(ctx, submit(honest, url, groundTruth[url], DeriveSigningKey(refreshSecret(honest)), now))
		if !out.Match {
			t.Fatalf("honest submission failed verification: %s", out.Detail)
		}
		// Dishonest node: claims DIFFERENT content than the trusted
		// re-fetch produces (poisoned/spam content), but signs it properly
		// — the signature is valid, the content is a lie. This is exactly
		// the case signature-only checking misses and re-crawl catches.
		evilBody := []byte("<html><h1>SPAM - buy now cheap pills</h1><p>totally different spam body text</p></html>")
		evilSub := submit(evil, url, evilBody, DeriveSigningKey(refreshSecret(evil)), now)
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

// TestFuzzyMatchHonestNode is F1: the live web is dynamic. The verifier's
// re-fetch of a page the node honestly crawled will differ slightly
// (session tokens, ads, timestamps, whitespace). An HONEST node must still
// pass — byte-exact SHA-256 comparison would have failed it.
func TestFuzzyMatchHonestNode(t *testing.T) {
	ctx := context.Background()
	nodeBody := []byte("<html><head><title>News</title></head><body><h1>Breaking story</h1><p>Article text here. <span class=\"ad\">ad-12345</span> <input type=\"hidden\" name=\"csrf\" value=\"tok-abc\"/></p></body></html>")
	// The verifier re-fetches a minute later: different ad id, rotated CSRF
	// token, extra whitespace — but the same page.
	refetchBody := []byte("<html>\n<head><title>News</title></head>\n<body><h1>Breaking story</h1><p>Article text here. <span class=\"ad\">ad-67890</span> <input type=\"hidden\" name=\"csrf\" value=\"tok-def\"/></p></body></html>")
	if ContentHash(nodeBody) == ContentHash(refetchBody) {
		t.Fatal("test setup: bodies must differ at the byte level")
	}
	pages := map[string][]byte{"https://example.com/news": refetchBody}
	scorer := NewTrustScorer()
	v := NewVerifier(trustedFetcher(pages), scorer, serverKeyFor, 1.0, time.Hour)

	out := v.VerifyOne(ctx, submit("node-1", "https://example.com/news", nodeBody, DeriveSigningKey(refreshSecret("node-1")), time.Now().Truncate(time.Second)))
	if !out.Match {
		t.Fatalf("honest node failed on a slightly-different re-fetch (F1): %s", out.Detail)
	}

	// Sanity: a genuinely different page must still fail.
	spam := []byte("<html><body><h1>FREE PILLS</h1><p>buy now cheap</p></body></html>")
	out = v.VerifyOne(ctx, submit("node-1", "https://example.com/news", spam, DeriveSigningKey(refreshSecret("node-1")), time.Now().Truncate(time.Second)))
	if out.Match {
		t.Fatal("spam body passed the fuzzy comparison (F1 too loose)")
	}
}

// TestFetchErrorIsInconclusive is F2: a trusted re-fetch failure is usually
// verifier-side (target down for us, rate-limited, geo-blocked, transient)
// and is attacker-exploitable (pick URLs that block the verifier's egress
// IP). It must be INCONCLUSIVE — no outcome recorded, no trust penalty.
func TestFetchErrorIsInconclusive(t *testing.T) {
	ctx := context.Background()
	errBoom := errors.New("connection refused")
	fetcher := func(ctx context.Context, url string) ([]byte, error) { return nil, errBoom }
	scorer := NewTrustScorer()
	v := NewVerifier(fetcher, scorer, serverKeyFor, 1.0, time.Hour)

	// Give the node a perfect record first.
	for i := 0; i < MinOutcomesBeforeFull; i++ {
		scorer.RecordVerification("node-1", true)
	}
	before := scorer.Score("node-1")
	outcomesBefore := scorer.Outcomes("node-1")

	body := []byte("<html><h1>real</h1></html>")
	out := v.VerifyOne(ctx, submit("node-1", "https://example.com/", body, DeriveSigningKey(refreshSecret("node-1")), time.Now()))
	if !out.Inconclusive {
		t.Fatal("fetch error was not marked inconclusive (F2)")
	}
	if out.Match {
		t.Fatal("fetch error counted as a match")
	}
	if got := scorer.Outcomes("node-1"); got != outcomesBefore {
		t.Fatalf("inconclusive outcome recorded: outcomes %d -> %d", outcomesBefore, got)
	}
	if got := scorer.Score("node-1"); got != before {
		t.Fatalf("inconclusive outcome moved the score: %.4f -> %.4f", before, got)
	}
}

// TestVerifierConcurrent is F3: Verifier.rng was not concurrency-safe; the
// ingest path verifies concurrent submissions. This must pass under -race.
func TestVerifierConcurrent(t *testing.T) {
	ctx := context.Background()
	body := []byte("<html><h1>concurrent test page with enough text to fingerprint</h1></html>")
	pages := map[string][]byte{"https://example.com/": body}
	scorer := NewTrustScorer()
	v := NewVerifier(trustedFetcher(pages), scorer, serverKeyFor, 1.0, 0)

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				sub := submit("node-c", "https://example.com/", body, DeriveSigningKey(refreshSecret("node-c")), time.Now())
				if _, _, err := v.MaybeVerify(ctx, sub); err != nil {
					t.Errorf("goroutine %d: %v", g, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if got := scorer.Outcomes("node-c"); got == 0 {
		t.Fatal("concurrent verifications recorded nothing")
	}
}

// TestEarnTrustThenDefect is F4: a node behaves for 25+ verifications to
// reach full, then poisons. Full-tier nodes stay sampled at the same rate,
// so the defection must be caught and the node must fall out of full
// within a bounded number of SAMPLED defections.
func TestEarnTrustThenDefect(t *testing.T) {
	ctx := context.Background()
	good := []byte("<html><h1>honest page with real content for fingerprinting</h1><p>paragraph one</p></html>")
	pages := map[string][]byte{"https://example.com/": good}
	scorer := NewTrustScorer()
	v := NewVerifier(trustedFetcher(pages), scorer, serverKeyFor, 1.0, 0)
	now := time.Now().Truncate(time.Second)

	// Earn trust: 30 perfect verifications.
	for i := 0; i < 30; i++ {
		out := v.VerifyOne(ctx, submit("defector", "https://example.com/", good, DeriveSigningKey(refreshSecret("defector")), now))
		if !out.Match {
			t.Fatalf("honest phase failed: %s", out.Detail)
		}
	}
	if got := scorer.Tier("defector"); got != TierFull {
		t.Fatalf("defector should have reached full, got %v", got)
	}

	// Defect: submit poisoned content. Each sampled defection must be a
	// recorded miss, and the node must fall out of full quickly.
	poison := []byte("<html><h1>POISONED index entry</h1><p>malicious totally different content</p></html>")
	defections := 0
	for i := 0; i < 25; i++ {
		out := v.VerifyOne(ctx, submit("defector", "https://example.com/", poison, DeriveSigningKey(refreshSecret("defector")), now))
		if out.Inconclusive {
			t.Fatal("poisoned submission must be conclusive")
		}
		if !out.Match {
			defections++
		}
		if scorer.Tier("defector") != TierFull {
			break
		}
	}
	if got := scorer.Tier("defector"); got == TierFull {
		t.Fatalf("defector still full after %d sampled defections", defections)
	}
	t.Logf("defector exited full after %d sampled defections", defections)
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
	v := NewVerifier(trustedFetcher(pages), scorer, serverKeyFor, 1.0, time.Minute)
	old := submit("node-1", "https://example.com/", pages["https://example.com/"], DeriveSigningKey(refreshSecret("node-1")), time.Now().Add(-time.Hour))
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
		t.Fatalf("bad-quality node still full (score=%.3f)", scorer.Score("node-1"))
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
