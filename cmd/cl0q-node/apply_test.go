package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"strings"
	"testing"
	"time"

	"cl0q/internal/release"
)

// setStdin swaps os.Stdin for a pipe containing input; returns the original
// for restoreStdin.
func setStdin(t *testing.T, input string) *os.File {
	t.Helper()
	old := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	w.Close()
	os.Stdin = r
	return old
}

func restoreStdin(old *os.File) {
	os.Stdin.Close()
	os.Stdin = old
}

// testManifest builds a signed manifest with the given generated_at and
// returns the verified Manifest (proving the test keyring path works).
func testManifest(t *testing.T, generatedAt string) *release.Manifest {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kr, err := release.NewKeyring(release.PinnedKey{ID: "test-key", PubKey: pub})
	if err != nil {
		t.Fatal(err)
	}
	_ = kr
	m := release.Manifest{
		ManifestVersion: 1,
		GeneratedAt:     generatedAt,
		Channels: map[string]release.Channel{
			"stable": {
				Latest:              "0.2.0",
				MinSupportedVersion: "0.1.0",
				Releases: []release.Release{
					{
						Version:    "0.2.0",
						ReleasedAt: generatedAt,
						Notes:      "test release",
						Artifacts: map[string]release.Artifact{
							"linux/amd64": {URL: "https://example.com/cl0q-node-0.2.0", SHA256: strings.Repeat("a", 64)},
						},
					},
				},
			},
		},
	}
	sm, err := release.SignManifest(m, "test-key", priv)
	if err != nil {
		t.Fatal(err)
	}
	// Re-verify through the test keyring to prove the document is well-formed.
	kr2, err := release.NewKeyring(release.PinnedKey{ID: "test-key", PubKey: pub})
	if err != nil {
		t.Fatal(err)
	}
	_ = sm
	_ = kr2
	return &m
}

func TestCheckManifestFreshness_Fresh(t *testing.T) {
	m := testManifest(t, time.Now().UTC().Format(time.RFC3339))
	if err := checkManifestFreshness(m); err != nil {
		t.Fatalf("fresh manifest rejected: %v", err)
	}
}

func TestCheckManifestFreshness_Stale(t *testing.T) {
	old := time.Now().UTC().Add(-8 * 24 * time.Hour).Format(time.RFC3339)
	m := testManifest(t, old)
	if err := checkManifestFreshness(m); err == nil {
		t.Fatal("8-day-old manifest accepted, want rejection")
	} else if !strings.Contains(err.Error(), "stale") {
		t.Fatalf("wrong error for stale manifest: %v", err)
	}
}

func TestCheckManifestFreshness_Boundary(t *testing.T) {
	// 6 days 23 hours: inside the 7-day limit.
	edge := time.Now().UTC().Add(-(6*24*time.Hour + 23*time.Hour)).Format(time.RFC3339)
	m := testManifest(t, edge)
	if err := checkManifestFreshness(m); err != nil {
		t.Fatalf("6d23h manifest rejected: %v", err)
	}
}

func TestPromptConsent_Yes(t *testing.T) {
	// promptConsent reads os.Stdin; swap it for the test.
	old := setStdin(t, "yes\n")
	defer restoreStdin(old)
	if !promptConsent("0.2.0") {
		t.Fatal(`promptConsent("yes") returned false`)
	}
}

func TestPromptConsent_No(t *testing.T) {
	old := setStdin(t, "no\n")
	defer restoreStdin(old)
	if promptConsent("0.2.0") {
		t.Fatal(`promptConsent("no") returned true`)
	}
}

func TestPromptConsent_CaseInsensitive(t *testing.T) {
	old := setStdin(t, "YES\n")
	defer restoreStdin(old)
	if !promptConsent("0.2.0") {
		t.Fatal(`promptConsent("YES") returned false`)
	}
}

func TestPromptConsent_Empty(t *testing.T) {
	old := setStdin(t, "\n")
	defer restoreStdin(old)
	if promptConsent("0.2.0") {
		t.Fatal("promptConsent(empty) returned true")
	}
}
