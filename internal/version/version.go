// Package version holds the node release version, stamped at build time
// via -ldflags "-X cl0q/internal/version.Version=...". Defaults to "dev"
// for un-stamped builds.
package version

// Version is the release version (VERSION file). Set by the Makefile.
var Version = "dev"
