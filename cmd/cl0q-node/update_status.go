// Command `cl0q-node update status`: Track U rung U4 — update-channel status.
//
// Reports the node's position on the update channel: the running binary's
// version, the channel's latest release, whether an update is available,
// whether the RUNNING version has been yanked upstream (critical), whether
// the running version still meets the channel's min_supported_version, and
// the last version this node successfully applied (U3 state file).
//
// Yank semantics: a yanked release is never served as an update target
// (U1 LatestRelease). But a node may already RUN a version that later gets
// yanked (e.g. a critical bug found post-release). `update status` surfaces
// that loudly so the operator upgrades immediately. It does not auto-update:
// consent (U3) still gates every apply.
//
// Min-version semantics: U1's Supported() reports whether the running
// version meets the channel's min_supported_version. Nodes below it get a
// loud warning here; enforcement (refusing to serve traffic) is a future
// rung, not this one — a warning that bricks nothing.
//
// Usage:
//
//	cl0q-node update status
//	cl0q-node update status --channel edge
//	cl0q-node update status --json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"codeberg.org/cl0qsearch/crawler-node/internal/release"
	"codeberg.org/cl0qsearch/crawler-node/internal/version"
)

// updateStatusReport is the machine-readable update-channel status.
type updateStatusReport struct {
	RunningVersion    string `json:"running_version"`
	Channel           string `json:"channel"`
	LatestAvailable   string `json:"latest_available"`
	UpdateAvailable   bool   `json:"update_available"`
	RunningYanked     bool   `json:"running_yanked"`
	MinSupported      string `json:"min_supported_version,omitempty"`
	MeetsMinSupported *bool  `json:"meets_min_supported,omitempty"`
	LastApplied       string `json:"last_applied,omitempty"`
	Warning           string `json:"warning,omitempty"`
}

func runUpdateStatusCommand(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	manifestURL := fs.String("manifest-url", "", "releases.json URL (default: CL0Q_RELEASE_MANIFEST_URL or the provisional U5 URL)")
	channel := fs.String("channel", "stable", "release channel to report on")
	jsonOut := fs.Bool("json", false, "output as JSON")
	_ = fs.Parse(args)

	m, _ := fetchVerifiedManifest(*manifestURL)
	rep, err := buildUpdateStatusReport(m, *channel, version.Version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update status: %v\n", err)
		os.Exit(1)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return
	}

	fmt.Printf("Running version: %s (channel %s)\n", rep.RunningVersion, rep.Channel)
	fmt.Printf("Latest available: %s\n", rep.LatestAvailable)
	if rep.UpdateAvailable {
		fmt.Printf("Update available: %s -> %s\n", rep.RunningVersion, rep.LatestAvailable)
	} else {
		fmt.Printf("Up to date.\n")
	}
	if rep.RunningYanked {
		fmt.Printf("CRITICAL: running version %s has been YANKED upstream — upgrade immediately\n", rep.RunningVersion)
	}
	if rep.MeetsMinSupported != nil && !*rep.MeetsMinSupported {
		fmt.Printf("WARNING: running version %s is below min_supported_version %s\n", rep.RunningVersion, rep.MinSupported)
	}
	if rep.LastApplied != "" {
		fmt.Printf("Last applied: %s\n", rep.LastApplied)
	}
	if rep.Warning != "" {
		fmt.Printf("Note: %s\n", rep.Warning)
	}
}

// buildUpdateStatusReport is a pure function of (manifest, channel,
// runningVersion) for testability.
func buildUpdateStatusReport(m *release.Manifest, channel, runningVersion string) (*updateStatusReport, error) {
	rel, err := m.LatestRelease(channel)
	if err != nil {
		return nil, err
	}
	rep := &updateStatusReport{
		Channel:         channel,
		RunningVersion:  runningVersion,
		LatestAvailable: rel.Version,
		LastApplied:     readAppliedVersion(),
	}

	// Yank check: is the RUNNING version yanked in this manifest?
	ch := m.Channels[channel]
	for i := range ch.Releases {
		if ch.Releases[i].Version == runningVersion && ch.Releases[i].Yanked {
			rep.RunningYanked = true
			break
		}
	}

	// Unstamped dev builds: report available, skip version comparisons.
	if runningVersion == "dev" || runningVersion == "" {
		rep.UpdateAvailable = true
		rep.Warning = "unstamped dev build; yank/min-supported checks need a stamped version"
		return rep, nil
	}

	rep.UpdateAvailable = release.CompareVersions(runningVersion, rel.Version) < 0
	rep.MinSupported = ch.MinSupportedVersion
	supported, err := m.Supported(channel, runningVersion)
	if err != nil {
		rep.Warning = fmt.Sprintf("cannot check min-supported: %v", err)
		return rep, nil
	}
	rep.MeetsMinSupported = &supported
	return rep, nil
}
