package main

import (
	"testing"

	"codeberg.org/cl0qsearch/crawler-node/internal/release"
)

func testUpdateManifest() *release.Manifest {
	return &release.Manifest{
		ManifestVersion: 1,
		GeneratedAt:     "2026-10-11T20:00:00Z",
		Channels: map[string]release.Channel{
			"stable": {
				Latest:              "0.2.0",
				MinSupportedVersion: "0.1.0",
				Releases: []release.Release{
					{
						Version: "0.2.0", ReleasedAt: "2026-10-11T20:00:00Z",
						Notes: "Bug fixes.", EgressChanges: "none",
						Artifacts: map[string]release.Artifact{
							"linux/amd64": {URL: "https://example.com/0.2.0", SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
						},
					},
					{
						Version: "0.1.0", ReleasedAt: "2026-10-01T00:00:00Z",
						Notes: "First release.",
						Artifacts: map[string]release.Artifact{
							"linux/amd64": {URL: "https://example.com/0.1.0", SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
						},
					},
				},
			},
		},
	}
}

func TestUpdateBehind(t *testing.T) {
	rep, err := buildUpdateReport(testUpdateManifest(), "stable", "0.1.0")
	if err != nil {
		t.Fatalf("buildUpdateReport: %v", err)
	}
	if !rep.Available || rep.From != "0.1.0" || rep.To != "0.2.0" {
		t.Errorf("got available=%v from=%q to=%q, want true 0.1.0->0.2.0", rep.Available, rep.From, rep.To)
	}
	if rep.Supported == nil || !*rep.Supported {
		t.Errorf("0.1.0 should be supported (min 0.1.0)")
	}
	if rep.Notes != "Bug fixes." || rep.EgressDelta != "none" {
		t.Errorf("notes/egress not carried: %q %q", rep.Notes, rep.EgressDelta)
	}
}

func TestUpdateCurrent(t *testing.T) {
	rep, err := buildUpdateReport(testUpdateManifest(), "stable", "0.2.0")
	if err != nil {
		t.Fatalf("buildUpdateReport: %v", err)
	}
	if rep.Available {
		t.Errorf("0.2.0 == latest: should not report available")
	}
}

func TestUpdateBelowMinSupported(t *testing.T) {
	rep, err := buildUpdateReport(testUpdateManifest(), "stable", "0.0.5")
	if err != nil {
		t.Fatalf("buildUpdateReport: %v", err)
	}
	if !rep.Available {
		t.Errorf("0.0.5 < 0.2.0: should report available")
	}
	if rep.Supported == nil || *rep.Supported {
		t.Errorf("0.0.5 < min 0.1.0: should be unsupported")
	}
	if rep.Warning == "" {
		t.Errorf("unsupported node should carry a loud warning")
	}
}

func TestUpdateDevBuild(t *testing.T) {
	rep, err := buildUpdateReport(testUpdateManifest(), "stable", "dev")
	if err != nil {
		t.Fatalf("buildUpdateReport: %v", err)
	}
	if !rep.Available || rep.Warning == "" {
		t.Errorf("dev build: want available=true with unstamped warning, got %+v", rep)
	}
}

func TestUpdateUnknownChannel(t *testing.T) {
	if _, err := buildUpdateReport(testUpdateManifest(), "nope", "0.1.0"); err == nil {
		t.Errorf("unknown channel should error")
	}
}

func TestUpdateYankedLatest(t *testing.T) {
	m := testUpdateManifest()
	m.Channels["stable"].Releases[0].Yanked = true
	if _, err := buildUpdateReport(m, "stable", "0.1.0"); err == nil {
		t.Errorf("yanked latest should error, not offer the yanked bytes")
	}
}
