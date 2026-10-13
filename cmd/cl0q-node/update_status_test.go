package main

import (
	"strings"
	"testing"
	"time"

	"cl0q/internal/release"
)

// statusTestManifest builds a manifest with a yanked old release.
func statusTestManifest(t *testing.T) *release.Manifest {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	return &release.Manifest{
		ManifestVersion: 1,
		GeneratedAt:     now,
		Channels: map[string]release.Channel{
			"stable": {
				Latest:              "0.3.0",
				MinSupportedVersion: "0.2.0",
				Releases: []release.Release{
					{
						Version:    "0.1.0",
						ReleasedAt: now,
						Notes:      "old",
						Yanked:     true,
						Artifacts: map[string]release.Artifact{
							"linux/amd64": {URL: "https://example.com/0.1.0", SHA256: strings.Repeat("b", 64)},
						},
					},
					{
						Version:    "0.2.0",
						ReleasedAt: now,
						Notes:      "current",
						Artifacts: map[string]release.Artifact{
							"linux/amd64": {URL: "https://example.com/0.2.0", SHA256: strings.Repeat("c", 64)},
						},
					},
					{
						Version:    "0.3.0",
						ReleasedAt: now,
						Notes:      "latest",
						Artifacts: map[string]release.Artifact{
							"linux/amd64": {URL: "https://example.com/0.3.0", SHA256: strings.Repeat("d", 64)},
						},
					},
				},
			},
		},
	}
}

func TestUpdateStatus_UpToDate(t *testing.T) {
	m := statusTestManifest(t)
	rep, err := buildUpdateStatusReport(m, "stable", "0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	if rep.UpdateAvailable {
		t.Fatal("0.3.0 reported update available, want up-to-date")
	}
	if rep.RunningYanked {
		t.Fatal("0.3.0 reported yanked, want false")
	}
	if rep.MeetsMinSupported == nil || !*rep.MeetsMinSupported {
		t.Fatal("0.3.0 should meet min_supported 0.2.0")
	}
}

func TestUpdateStatus_RunningYanked(t *testing.T) {
	m := statusTestManifest(t)
	rep, err := buildUpdateStatusReport(m, "stable", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.RunningYanked {
		t.Fatal("0.1.0 yank not detected")
	}
	if !rep.UpdateAvailable {
		t.Fatal("0.1.0 should show update available to 0.3.0")
	}
}

func TestUpdateStatus_BelowMinSupported(t *testing.T) {
	m := statusTestManifest(t)
	rep, err := buildUpdateStatusReport(m, "stable", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if rep.MeetsMinSupported == nil || *rep.MeetsMinSupported {
		t.Fatal("0.1.0 should NOT meet min_supported 0.2.0")
	}
	if rep.MinSupported != "0.2.0" {
		t.Fatalf("MinSupported = %q, want 0.2.0", rep.MinSupported)
	}
}

func TestUpdateStatus_DevBuild(t *testing.T) {
	m := statusTestManifest(t)
	rep, err := buildUpdateStatusReport(m, "stable", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.UpdateAvailable {
		t.Fatal("dev build should report update available")
	}
	if rep.Warning == "" {
		t.Fatal("dev build should carry a warning")
	}
}

func TestUpdateStatus_UnknownChannel(t *testing.T) {
	m := statusTestManifest(t)
	if _, err := buildUpdateStatusReport(m, "nope", "0.2.0"); err == nil {
		t.Fatal("unknown channel accepted, want error")
	}
}
