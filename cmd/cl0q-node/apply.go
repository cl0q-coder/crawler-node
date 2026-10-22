// Command `cl0q-node update apply`: Track U rung U3 — consent-gated update apply.
//
// Fetches the signed release manifest (releases.json), verifies it against
// the keys PINNED in this binary (fail-closed, same as `update`), and —
// only after the operator explicitly agrees — downloads the new binary,
// verifies its SHA-256 against the manifest, swaps it in atomically, runs
// a health check, and rolls back automatically on failure.
//
// Consent model (PROTOCOL.md): the operator sees current→new version, the
// human release notes, and any egress/permission/privacy change BEFORE any
// byte is downloaded. Nothing downloads without explicit agreement: either
// an interactive "yes" at the prompt or --yes on the command line (for
// automation that has already shown the operator the notes).
//
// Safety properties:
//   - Manifest verified with U1's ParseAndVerify before any field is trusted.
//   - Artifact URL must be https (manifest schema enforces it); download is
//     size-capped.
//   - SHA-256 of the downloaded bytes must match the manifest exactly.
//   - The running binary is never overwritten in place: the new binary is
//     written to a temp file in the same directory, fsynced, chmod +x, then
//     atomically renamed over the target. The previous binary is kept as
//     <binary>.bak until the health check passes.
//   - Health check: the new binary must execute `<new> --version` and exit 0
//     within 15 seconds. On any failure the backup is restored.
//   - Freshness guard (#273): a manifest older than 7 days is rejected, and
//     applying a version <= the last-applied version (tracked in the state
//     file) is refused — no silent downgrades or replays.
//   - Never applies a yanked release (LatestRelease already excludes those).
//
// Usage:
//
//	cl0q-node update apply                  # interactive consent prompt
//	cl0q-node update apply --yes            # non-interactive (automation)
//	cl0q-node update apply --channel edge
//	cl0q-node update apply --manifest-url https://.../releases.json
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"codeberg.org/cl0qsearch/crawler-node/internal/release"
	"codeberg.org/cl0qsearch/crawler-node/internal/version"
)

// applyStateFile records the last successfully applied version so a stale
// or replayed manifest cannot silently downgrade the node (#273).
const applyStateFileName = ".cl0q-update-state"

// maxManifestAge is the freshness guard: manifests older than this are
// rejected outright (#273). Operators should always see a recent manifest.
const maxManifestAge = 7 * 24 * time.Hour

// maxArtifactBytes caps the download (100 MiB — far above any real node
// binary, far below anything that could fill a disk by accident).
const maxArtifactBytes = 100 << 20

func runUpdateApplyCommand(args []string) {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	manifestURL := fs.String("manifest-url", "", "releases.json URL (default: CL0Q_RELEASE_MANIFEST_URL or the provisional U5 URL)")
	channel := fs.String("channel", "stable", "release channel to apply from")
	yes := fs.Bool("yes", false, "non-interactive: agree without prompting (automation only; operator must have seen the notes)")
	_ = fs.Parse(args)

	m, _ := fetchVerifiedManifest(*manifestURL)
	rep, err := buildUpdateReport(m, *channel, version.Version)
	if err != nil {
		fatalf("update apply: %v", err)
	}
	if !rep.Available {
		fmt.Printf("Already up to date: %s (channel %s, latest %s)\n", rep.From, rep.Channel, rep.To)
		return
	}

	// Freshness guard (#273): refuse stale manifests.
	if err := checkManifestFreshness(m); err != nil {
		fatalf("update apply: %v", err)
	}

	// Replay/downgrade guard (#273): refuse to apply at or below the last
	// successfully applied version.
	if err := checkNoReplay(rep.To); err != nil {
		fatalf("update apply: %v", err)
	}

	// Consent: show everything BEFORE any download.
	fmt.Printf("Update available: %s -> %s (channel %s)\n", rep.From, rep.To, rep.Channel)
	if rep.Notes != "" {
		fmt.Printf("Release notes:\n%s\n", rep.Notes)
	}
	if rep.EgressDelta != "" {
		fmt.Printf("EGRESS/PERMISSION CHANGES:\n%s\n", rep.EgressDelta)
	} else {
		fmt.Printf("Egress/permission changes: none declared\n")
	}
	if rep.Warning != "" {
		fmt.Printf("WARNING: %s\n", rep.Warning)
	}
	if !*yes {
		if !promptConsent(rep.To) {
			fmt.Println("Aborted: no consent given, nothing downloaded.")
			return
		}
	}

	rel, err := m.LatestRelease(*channel)
	if err != nil {
		fatalf("update apply: %v", err)
	}
	platform := runtime.GOOS + "/" + runtime.GOARCH
	art, ok := rel.Artifacts[platform]
	if !ok {
		fatalf("update apply: release %s has no artifact for %s", rel.Version, platform)
	}

	self, err := os.Executable()
	if err != nil {
		fatalf("update apply: cannot locate own binary: %v", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		fatalf("update apply: cannot resolve own binary: %v", err)
	}

	fmt.Printf("Downloading %s ...\n", art.URL)
	data := downloadArtifact(art.URL)
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != strings.ToLower(rel.Artifacts[platform].SHA256) {
		fatalf("update apply: SHA-256 mismatch for %s — refusing to apply (manifest may be stale or the download was tampered with)", art.URL)
	}
	fmt.Printf("SHA-256 verified (%d bytes).\n", len(data))

	applyAtomic(self, data, rel.Version)
	recordAppliedVersion(self, rel.Version)
	fmt.Printf("Updated to %s. Previous binary kept at %s.bak until next apply.\n", rel.Version, self)
}

// promptConsent asks the operator for explicit agreement. Returns true only
// on an unambiguous "yes".
func promptConsent(to string) bool {
	fmt.Printf("Apply update to %s? Type \"yes\" to agree: ", to)
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}
	return strings.TrimSpace(strings.ToLower(scanner.Text())) == "yes"
}

// downloadArtifact fetches the artifact over https with a size cap.
func downloadArtifact(rawURL string) []byte {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		fatalf("update apply: artifact URL must be https: %q", rawURL)
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(rawURL)
	if err != nil {
		fatalf("update apply: download failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fatalf("update apply: download: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxArtifactBytes+1))
	if err != nil {
		fatalf("update apply: download read: %v", err)
	}
	if len(data) > maxArtifactBytes {
		fatalf("update apply: artifact exceeds %d bytes, refusing", maxArtifactBytes)
	}
	return data
}

// applyAtomic swaps the new binary in atomically with health check and
// automatic rollback. The previous binary is preserved as <self>.bak.
func applyAtomic(self string, data []byte, newVersion string) {
	dir := filepath.Dir(self)
	tmp, err := os.CreateTemp(dir, ".cl0q-update-*")
	if err != nil {
		fatalf("update apply: temp file: %v", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		fatalf("update apply: writing new binary: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		fatalf("update apply: fsync: %v", err)
	}
	tmp.Close()
	if err := os.Chmod(tmpName, 0755); err != nil {
		fatalf("update apply: chmod: %v", err)
	}

	// Health check BEFORE the swap: the new binary must run --version.
	if err := healthCheck(tmpName); err != nil {
		fatalf("update apply: new binary failed health check: %v — nothing was changed", err)
	}

	backup := self + ".bak"
	// Move the current binary aside (keep the previous .bak if apply fails
	// midway; a successful apply overwrites it).
	if err := os.Rename(self, backup); err != nil {
		fatalf("update apply: backing up current binary: %v", err)
	}
	if err := os.Rename(tmpName, self); err != nil {
		// Swap failed after backup: restore immediately.
		_ = os.Rename(backup, self)
		fatalf("update apply: atomic swap failed, restored backup: %v", err)
	}

	// Health check AFTER the swap, on the final path.
	if err := healthCheck(self); err != nil {
		_ = os.Rename(backup, self)
		fatalf("update apply: swapped binary failed health check, rolled back: %v", err)
	}
}

// healthCheck runs `bin --version` and requires exit 0 within 15s.
func healthCheck(bin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

// checkManifestFreshness rejects manifests older than maxManifestAge (#273).
func checkManifestFreshness(m *release.Manifest) error {
	gen, err := time.Parse(time.RFC3339, m.GeneratedAt)
	if err != nil {
		return fmt.Errorf("bad manifest generated_at: %w", err)
	}
	if age := time.Since(gen); age > maxManifestAge {
		return fmt.Errorf("manifest is %s old (generated %s) — older than the %s freshness limit; refusing to apply from a stale manifest", age.Round(time.Hour), m.GeneratedAt, maxManifestAge)
	}
	return nil
}

// checkNoReplay refuses versions at or below the last applied one (#273).
func checkNoReplay(to string) error {
	last := readAppliedVersion()
	if last == "" {
		return nil
	}
	if release.CompareVersions(to, last) <= 0 {
		return fmt.Errorf("refusing to apply %s: last applied version is %s (no downgrades or replays)", to, last)
	}
	return nil
}

// stateDir returns the directory holding the apply state file: alongside
// the running binary.
func stateDir() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return ""
	}
	return filepath.Dir(self)
}

func statePathFor(self string) string {
	return filepath.Join(filepath.Dir(self), applyStateFileName)
}

func readAppliedVersion() string {
	dir := stateDir()
	if dir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, applyStateFileName))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func recordAppliedVersion(self, v string) {
	p := statePathFor(self)
	// Best effort: a failed state write must not fail the apply, but warn.
	if err := os.WriteFile(p, []byte(v+"\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record applied version: %v\n", err)
	}
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
