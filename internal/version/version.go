// Package version holds the node release version, stamped at build time
// via -ldflags "-X cl0q/internal/version.Version=... -X cl0q/internal/version.Commit=... -X cl0q/internal/version.BuildTime=...".
// Defaults are "dev"/"unknown" for un-stamped builds.
//
// The provenance model (Track O, O2/O3): a stamp is only meaningful when it
// names the exact source commit the binary was built from, on a clean tree.
// The Makefile's build target refuses dirty trees, so commit != "unknown"
// implies the binary came from that canonical commit.
package version

import (
	"fmt"
	"os"
)

// Version is the release version (VERSION file). Set by the Makefile.
var Version = "dev"

// Commit is the short git SHA of the source commit. Set by the Makefile.
var Commit = "unknown"

// BuildTime is the UTC build timestamp (RFC3339). Set by the Makefile.
var BuildTime = "unknown"

// Report returns the one-line provenance report every binary prints for
// -version/--version: version, source commit, and build time.
func Report() string {
	return fmt.Sprintf("version=%s commit=%s built=%s", Version, Commit, BuildTime)
}

// HandleVersionFlag scans os.Args for -version/--version, prints the
// provenance report for the named binary, and reports whether it handled
// the invocation (caller should return immediately when true).
// Call this as the very first thing in main, before any config validation,
// so --version works without a configured environment.
func HandleVersionFlag(name string) bool {
	for _, a := range os.Args[1:] {
		if a == "-version" || a == "--version" {
			fmt.Println(name, Report())
			return true
		}
	}
	return false
}
