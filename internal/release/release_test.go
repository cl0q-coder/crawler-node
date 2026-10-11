package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// testPrivKeys holds the TEST private keys matching TestPinnedKeys.
// Fixtures only — never production key material (see keys.go).
var testPrivKeys = map[string]ed25519.PrivateKey{}

func testPriv(t *testing.T, keyID, seedB64 string) ed25519.PrivateKey {
	t.Helper()
	if k, ok := testPrivKeys[keyID]; ok {
		return k
	}
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil {
		t.Fatalf("bad test seed: %v", err)
	}
	k := ed25519.NewKeyFromSeed(seed)
	testPrivKeys[keyID] = k
	return k
}

func testPrivA(t *testing.T) ed25519.PrivateKey {
	return testPriv(t, "test-key-a", "F5vnbrPR3zWxnjwk5YxcuxM93W4MaW4EnKcAgvc/OOw=")
}

func testPrivB(t *testing.T) ed25519.PrivateKey {
	return testPriv(t, "test-key-b", "0+OtAg8oZ7H6v4ZDh9mKv3Crou96Wm4UUQ0oJ6pa6QQ=")
}

func testManifest() Manifest {
	return Manifest{
		ManifestVersion: 1,
		GeneratedAt:     "2026-10-11T20:00:00Z",
		Channels: map[string]Channel{
			"stable": {
				Latest:              "0.2.0",
				MinSupportedVersion: "0.1.0",
				Releases: []Release{
					{
						Version:    "0.2.0",
						ReleasedAt: "2026-10-11T20:00:00Z",
						Notes:      "Bug fixes and faster frontier sweep.",
						Artifacts: map[string]Artifact{
							"linux/amd64": {
								URL:    "https://releases.cl0q.com/stable/0.2.0/cl0q-node-linux-amd64",
								SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
							},
						},
					},
					{
						Version:    "0.1.0",
						ReleasedAt: "2026-10-01T00:00:00Z",
						Notes:      "First release.",
						Artifacts: map[string]Artifact{
							"linux/amd64": {
								URL:    "https://releases.cl0q.com/stable/0.1.0/cl0q-node-linux-amd64",
								SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
							},
						},
					},
				},
			},
		},
	}
}

func signedDoc(t *testing.T, m Manifest, keyID string, priv ed25519.PrivateKey) []byte {
	t.Helper()
	sm, err := SignManifest(m, keyID, priv)
	if err != nil {
		t.Fatalf("SignManifest: %v", err)
	}
	// Marshal the same way a publisher would: the full signed document.
	out := `{"manifest":` + mustMarshal(t, sm.Manifest) + `,"signatures":` + mustMarshal(t, sm.Signatures) + `}`
	return []byte(out)
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestValidManifestVerifies(t *testing.T) {
	kr := mustKeyring(t)
	data := signedDoc(t, testManifest(), "test-key-a", testPrivA(t))
	m, err := ParseAndVerify(data, kr)
	if err != nil {
		t.Fatalf("ParseAndVerify: %v", err)
	}
	rel, err := m.LatestRelease("stable")
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if rel.Version != "0.2.0" {
		t.Fatalf("latest = %q, want 0.2.0", rel.Version)
	}
}

func TestSecondPinnedKeyVerifies(t *testing.T) {
	kr := mustKeyring(t)
	data := signedDoc(t, testManifest(), "test-key-b", testPrivB(t))
	if _, err := ParseAndVerify(data, kr); err != nil {
		t.Fatalf("key B should verify: %v", err)
	}
}

func TestTamperedManifestRejected(t *testing.T) {
	kr := mustKeyring(t)
	data := signedDoc(t, testManifest(), "test-key-a", testPrivA(t))
	// Tamper: rewrite the notes after signing. The signature must fail.
	tampered := strings.Replace(string(data), "Bug fixes", "Evil fixes", 1)
	if _, err := ParseAndVerify([]byte(tampered), kr); err == nil {
		t.Fatal("tampered manifest verified; want rejection")
	}
}

func TestTamperedSignatureRejected(t *testing.T) {
	kr := mustKeyring(t)
	data := signedDoc(t, testManifest(), "test-key-a", testPrivA(t))
	// Corrupt the signature bytes themselves.
	corrupt := strings.Replace(string(data), `"signature":"`, `"signature":"AAAA`, 1)
	if _, err := ParseAndVerify([]byte(corrupt), kr); err == nil {
		t.Fatal("corrupt signature verified; want rejection")
	}
}

func TestUnknownKeyIDRejected(t *testing.T) {
	kr := mustKeyring(t)
	m := testManifest()
	sm, err := SignManifest(m, "test-key-a", testPrivA(t))
	if err != nil {
		t.Fatal(err)
	}
	sm.Signatures[0].KeyID = "someone-elses-key"
	data := []byte(`{"manifest":` + mustMarshal(t, sm.Manifest) + `,"signatures":` + mustMarshal(t, sm.Signatures) + `}`)
	if _, err := ParseAndVerify(data, kr); err == nil {
		t.Fatal("unknown key_id verified; want rejection")
	}
}

func TestNoSignaturesRejected(t *testing.T) {
	kr := mustKeyring(t)
	m := testManifest()
	data := []byte(`{"manifest":` + mustMarshal(t, m) + `,"signatures":[]}`)
	if _, err := ParseAndVerify(data, kr); err == nil {
		t.Fatal("unsigned manifest verified; want rejection")
	}
}

func TestBadJSONRejected(t *testing.T) {
	kr := mustKeyring(t)
	if _, err := ParseAndVerify([]byte(`{not json`), kr); err == nil {
		t.Fatal("bad JSON parsed; want rejection")
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	kr := mustKeyring(t)
	data := signedDoc(t, testManifest(), "test-key-a", testPrivA(t))
	withExtra := strings.Replace(string(data), `"manifest_version":1`, `"manifest_version":1,"sneaky":true`, 1)
	if _, err := ParseAndVerify([]byte(withExtra), kr); err == nil {
		t.Fatal("unknown field accepted; want rejection")
	}
}

func TestBadArtifactHashRejected(t *testing.T) {
	kr := mustKeyring(t)
	m := testManifest()
	m.Channels["stable"].Releases[0].Artifacts["linux/amd64"] = Artifact{
		URL:    "https://releases.cl0q.com/stable/0.2.0/x",
		SHA256: "not-a-hash",
	}
	data := signedDoc(t, m, "test-key-a", testPrivA(t))
	if _, err := ParseAndVerify(data, kr); err == nil {
		t.Fatal("bad sha256 accepted; want rejection")
	}
}

func TestNonHTTPSArtifactRejected(t *testing.T) {
	kr := mustKeyring(t)
	m := testManifest()
	m.Channels["stable"].Releases[0].Artifacts["linux/amd64"] = Artifact{
		URL:    "http://evil.example.com/x",
		SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	}
	data := signedDoc(t, m, "test-key-a", testPrivA(t))
	if _, err := ParseAndVerify(data, kr); err == nil {
		t.Fatal("http artifact accepted; want rejection")
	}
}

func TestYankedLatestIsError(t *testing.T) {
	kr := mustKeyring(t)
	m := testManifest()
	m.Channels["stable"].Releases[0].Yanked = true
	data := signedDoc(t, m, "test-key-a", testPrivA(t))
	parsed, err := ParseAndVerify(data, kr)
	if err != nil {
		t.Fatalf("yanked release should still parse: %v", err)
	}
	if _, err := parsed.LatestRelease("stable"); err == nil {
		t.Fatal("yanked latest returned as release; want error")
	}
}

func TestRotationKeepsOldNodesWorking(t *testing.T) {
	// Rotation scenario: the release pipeline starts signing with a NEW key
	// C that old nodes never pinned. Old nodes must REJECT it (fail closed),
	// while a manifest signed by still-pinned key B keeps verifying.
	oldKR := mustKeyring(t)
	data := signedDoc(t, testManifest(), "test-key-b", testPrivB(t))
	if _, err := ParseAndVerify(data, oldKR); err != nil {
		t.Fatalf("old keyring should verify B-signed manifest: %v", err)
	}

	// New key C: signed fine, but no pinned keyring knows it.
	_, privC, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sm, err := SignManifest(testManifest(), "key-c", privC)
	if err != nil {
		t.Fatal(err)
	}
	dataC := []byte(`{"manifest":` + mustMarshal(t, sm.Manifest) + `,"signatures":` + mustMarshal(t, sm.Signatures) + `}`)
	if _, err := ParseAndVerify(dataC, oldKR); err == nil {
		t.Fatal("unpinned key C verified on old keyring; want rejection")
	}

	// After the rotation ships (keyring gains C), the C-signed manifest verifies.
	newKR, err := NewKeyring(TestPinnedKeys[0], TestPinnedKeys[1], PinnedKey{ID: "key-c", PubKey: privC.Public().(ed25519.PublicKey)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseAndVerify(dataC, newKR); err != nil {
		t.Fatalf("C-signed manifest should verify once C is pinned: %v", err)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.2.0", -1},
		{"0.2.0", "0.2.0", 0},
		{"0.10.0", "0.9.0", 1}, // numeric, not lexical
		{"1.2", "1.2.0", 0},
		{"0.1.0", "0.1", 0},
		{"2.0.0", "10.0.0", -1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSupported(t *testing.T) {
	kr := mustKeyring(t)
	data := signedDoc(t, testManifest(), "test-key-a", testPrivA(t))
	m, err := ParseAndVerify(data, kr)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := m.Supported("stable", "0.1.0")
	if err != nil || !ok {
		t.Errorf("0.1.0 should be supported: %v %v", ok, err)
	}
	ok, err = m.Supported("stable", "0.0.9")
	if err != nil || ok {
		t.Errorf("0.0.9 should NOT be supported: %v %v", ok, err)
	}
	if _, err := m.Supported("nope", "0.1.0"); err == nil {
		t.Error("unknown channel should error")
	}
}

func mustKeyring(t *testing.T) *Keyring {
	t.Helper()
	kr, err := NewKeyring(TestPinnedKeys...)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	return kr
}
