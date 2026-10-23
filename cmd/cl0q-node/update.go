// Command `cl0q-node update`: Track U rung U2 — operator-facing update check.
//
// Fetches the signed release manifest (releases.json), verifies it against
// the keys PINNED in this binary (fail-closed: a tampered or MITM'd manifest
// is rejected outright), and compares the channel's latest release to this
// binary's stamped version. Prints {available, from→to, channel, notes,
// egress deltas}; warns loudly when this node is below the channel's
// min_supported_version.
//
// The manifest URL is set by the release pipeline (Track U rung U5); until
// then the default is provisional. Override with --manifest-url or
// CL0Q_RELEASE_MANIFEST_URL. Like the forwarder, https is required unless
// CL0Q_NODE_ALLOW_HTTP=1 (local test setups only).
//
// Usage:
//
//	cl0q-node update                        # check the stable channel
//	cl0q-node update --channel edge         # check another channel
//	cl0q-node update --json                 # JSON for scripting
//	cl0q-node update --manifest-url https://.../releases.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"codeberg.org/cl0qsearch/crawler-node/internal/release"
	"codeberg.org/cl0qsearch/crawler-node/internal/version"
)

// provisionalManifestURL is where rung U5 will publish releases.json. The
// exact URL is TBD with the release pipeline; this default is replaced
// then. --manifest-url / CL0Q_RELEASE_MANIFEST_URL win over the default.
const provisionalManifestURL = "https://codeberg.org/cl0qsearch/crawler-node/releases/download/manifest/releases.json"

// updateReport is the machine-readable result of the check.
type updateReport struct {
	Available   bool   `json:"available"`
	From        string `json:"from"`
	To          string `json:"to"`
	Channel     string `json:"channel"`
	Notes       string `json:"notes"`
	EgressDelta string `json:"egress_delta,omitempty"`
	Supported   *bool  `json:"supported,omitempty"` // nil when the build is unstamped
	Warning     string `json:"warning,omitempty"`
}

func runUpdateCommand(args []string) {
	// Subcommand: `cl0q-node update apply` (Track U rung U3).
	if len(args) > 0 && args[0] == "apply" {
		runUpdateApplyCommand(args[1:])
		return
	}
	// Subcommand: `cl0q-node update status` (Track U rung U4).
	if len(args) > 0 && args[0] == "status" {
		runUpdateStatusCommand(args[1:])
		return
	}

	fs := flag.NewFlagSet("update", flag.ExitOnError)
	manifestURL := fs.String("manifest-url", "", "releases.json URL (default: CL0Q_RELEASE_MANIFEST_URL or the provisional U5 URL)")
	channel := fs.String("channel", "stable", "release channel to check")
	jsonOut := fs.Bool("json", false, "output as JSON")
	_ = fs.Parse(args)

	m, _ := fetchVerifiedManifest(*manifestURL)

	rep, err := buildUpdateReport(m, *channel, version.Version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		os.Exit(1)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return
	}
	if rep.Available {
		fmt.Printf("Update available: %s -> %s (channel %s)\n", rep.From, rep.To, rep.Channel)
	} else {
		fmt.Printf("Up to date: %s (channel %s, latest %s)\n", rep.From, rep.Channel, rep.To)
	}
	if rep.Notes != "" {
		fmt.Printf("Notes: %s\n", rep.Notes)
	}
	if rep.EgressDelta != "" {
		fmt.Printf("Egress/permission changes: %s\n", rep.EgressDelta)
	}
	if rep.Warning != "" {
		fmt.Printf("WARNING: %s\n", rep.Warning)
	}
}

// fetchVerifiedManifest resolves the manifest URL (flag > env > provisional),
// fetches releases.json, and verifies it against the pinned keys. It exits
// non-zero on any failure: an unverified manifest is never trusted.
// Shared by `update` (rung U2) and `update apply` (rung U3).
func fetchVerifiedManifest(flagURL string) (*release.Manifest, string) {
	murl := flagURL
	if murl == "" {
		murl = os.Getenv("CL0Q_RELEASE_MANIFEST_URL")
	}
	if murl == "" {
		murl = provisionalManifestURL
	}
	u, err := url.Parse(murl)
	if err != nil || (u.Scheme != "https" && !(u.Scheme == "http" && os.Getenv("CL0Q_NODE_ALLOW_HTTP") == "1")) || u.Host == "" {
		fmt.Fprintf(os.Stderr, "update: bad manifest URL %q (need https, or http with CL0Q_NODE_ALLOW_HTTP=1)\n", murl)
		os.Exit(1)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(murl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: cannot fetch manifest at %s: %v\n", murl, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "update: manifest fetch at %s: HTTP %d\n", murl, resp.StatusCode)
		os.Exit(1)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: reading manifest: %v\n", err)
		os.Exit(1)
	}

	kr, err := release.NewKeyring(release.PinnedKeys...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: bad pinned keyring: %v\n", err)
		os.Exit(1)
	}
	m, err := release.ParseAndVerify(body, kr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: manifest REJECTED: %v\n", err)
		fmt.Fprintf(os.Stderr, "Refusing to trust any release info from an unverified manifest.\n")
		os.Exit(1)
	}
	return m, murl
}

// buildUpdateReport compares nodeVersion against the channel's latest
// release. Pure function of (manifest, channel, nodeVersion) so it is
// unit-testable without network.
func buildUpdateReport(m *release.Manifest, channel, nodeVersion string) (*updateReport, error) {
	rel, err := m.LatestRelease(channel)
	if err != nil {
		return nil, err
	}
	rep := &updateReport{
		Channel:     channel,
		From:        nodeVersion,
		To:          rel.Version,
		Notes:       rel.Notes,
		EgressDelta: rel.EgressChanges,
	}
	// Unstamped dev builds always report the release as available (they are
	// not a release), and skip the min-supported check (no version to check).
	if nodeVersion == "dev" || nodeVersion == "" {
		rep.Available = true
		rep.Warning = "this binary is unstamped (dev build); version comparison is against the release line only"
		return rep, nil
	}
	rep.Available = release.CompareVersions(nodeVersion, rel.Version) < 0
	supported, err := m.Supported(channel, nodeVersion)
	if err != nil {
		// Bad node version string: not comparable, say so plainly.
		rep.Warning = fmt.Sprintf("node version %q is not a dotted version; cannot check min-supported", nodeVersion)
		return rep, nil
	}
	rep.Supported = &supported
	if !supported {
		ch := m.Channels[channel]
		rep.Warning = fmt.Sprintf("node version %s is below min_supported_version %s — upgrade promptly; old nodes may be rejected",
			nodeVersion, ch.MinSupportedVersion)
	}
	return rep, nil
}
