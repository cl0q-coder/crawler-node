package version

import (
	"strings"
	"testing"
)

// The provenance report must always carry all three fields, even for
// un-stamped dev builds, so parsers (and the cl0q-provenance verify tool)
// can rely on the shape.
func TestReportShape(t *testing.T) {
	r := Report()
	for _, want := range []string{"version=", "commit=", "built="} {
		if !strings.Contains(r, want) {
			t.Fatalf("Report() = %q, missing %q", r, want)
		}
	}
}

// Un-stamped builds must be visibly un-stamped: a commit of "unknown" means
// "not built from a canonical commit" and must never be mistaken for one.
func TestUnstampedDefaults(t *testing.T) {
	if Version != "dev" || Commit != "unknown" || BuildTime != "unknown" {
		t.Fatalf("defaults changed: Version=%q Commit=%q BuildTime=%q",
			Version, Commit, BuildTime)
	}
}
