// Package release implements Track U rung U1: the signed release manifest.
//
// A crawler node operator must be able to check for updates, see human
// release notes, and pull an update only after explicit agreement. Because
// the update channel is a remote-code path onto every operator's box, the
// manifest that advertises releases is signed with Ed25519 and verified
// against keys PINNED in the node binary before any field is trusted.
// A tampered or MITM'd manifest is rejected outright (verify-on-parse).
//
// Wire format (releases.json):
//
//	{
//	  "manifest": {
//	    "manifest_version": 1,
//	    "generated_at": "2026-10-11T20:00:00Z",
//	    "channels": {
//	      "stable": {
//	        "latest": "0.2.0",
//	        "min_supported_version": "0.1.0",
//	        "releases": [
//	          {
//	            "version": "0.2.0",
//	            "released_at": "2026-10-11T20:00:00Z",
//	            "notes": "human release notes",
//	            "egress_changes": "explicit callout of any egress/permission/privacy change",
//	            "yanked": false,
//	            "artifacts": {
//	              "linux/amd64": {"url": "https://...", "sha256": "<64 hex chars>"}
//	            }
//	          }
//	        ]
//	      }
//	    }
//	  },
//	  "signatures": [
//	    {"key_id": "cl0q-release-2026a", "signature": "<base64 ed25519 sig>"}
//	  ]
//	}
//
// Canonical signing bytes: the exact bytes of json.Marshal(manifest).
// Go marshals struct fields in declaration order and sorts map keys, so
// this is deterministic for a given manifest value. The signature covers
// the "manifest" object only, never the "signatures" array.
//
// KEY CUSTODY (open decision): Chris has not yet decided where the private
// release-signing key lives or who may sign a release. Until then this
// package pins TEST keys only (see keys.go: TestPinnedKeys). Nothing here
// is production key material. Swapping in the real production public keys
// later is a one-line change (DefaultKeyring).
package release

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// currentManifestVersion is the only manifest schema version this code
// understands. Bump it if the schema ever changes incompatibly.
const currentManifestVersion = 1

// Manifest is the signed payload: every release the update channel knows
// about, grouped by channel (e.g. "stable", "edge").
type Manifest struct {
	ManifestVersion int                `json:"manifest_version"`
	GeneratedAt     string             `json:"generated_at"` // RFC3339
	Channels        map[string]Channel `json:"channels"`
}

// Channel is one release line (stable, edge, ...).
type Channel struct {
	Latest              string    `json:"latest"`
	MinSupportedVersion string    `json:"min_supported_version"`
	Releases            []Release `json:"releases"`
}

// Release is a single published node version.
type Release struct {
	Version       string              `json:"version"`
	ReleasedAt    string              `json:"released_at"` // RFC3339
	Notes         string              `json:"notes"`
	EgressChanges string              `json:"egress_changes,omitempty"`
	Yanked        bool                `json:"yanked"`
	Artifacts     map[string]Artifact `json:"artifacts"` // key: "GOOS/GOARCH"
}

// Artifact is one downloadable node binary.
type Artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"` // lowercase hex, 64 chars
}

// ManifestSignature is one Ed25519 signature over the canonical manifest
// bytes. KeyID names which pinned key signed (see Keyring).
type ManifestSignature struct {
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"` // base64 std encoding, 64 bytes
}

// SignedManifest is the full releases.json document: the manifest plus its
// signatures. Verify with ParseAndVerify — never trust Manifest fields from
// an unverified document.
type SignedManifest struct {
	Manifest   Manifest            `json:"manifest"`
	Signatures []ManifestSignature `json:"signatures"`
}

// canonicalBytes returns the exact bytes a signature must cover.
func canonicalBytes(m Manifest) ([]byte, error) {
	return json.Marshal(m)
}

// SignManifest signs m with priv and returns the signed document.
// Used by the release pipeline (Track U rung U5) and by tests. keyID must
// match the ID the nodes pin for the corresponding public key.
func SignManifest(m Manifest, keyID string, priv ed25519.PrivateKey) (SignedManifest, error) {
	if keyID == "" {
		return SignedManifest{}, fmt.Errorf("release: key_id must not be empty")
	}
	if len(priv) != ed25519.PrivateKeySize {
		return SignedManifest{}, fmt.Errorf("release: bad private key length %d", len(priv))
	}
	canonical, err := canonicalBytes(m)
	if err != nil {
		return SignedManifest{}, fmt.Errorf("release: marshal manifest: %w", err)
	}
	sig := ed25519.Sign(priv, canonical)
	return SignedManifest{
		Manifest: m,
		Signatures: []ManifestSignature{{
			KeyID:     keyID,
			Signature: base64.StdEncoding.EncodeToString(sig),
		}},
	}, nil
}

// ParseAndVerify parses a releases.json document and verifies it against
// the keyring. It returns the manifest only if at least one signature is
// valid for a PINNED key AND the schema validates. Everything else fails
// closed: bad JSON, unknown key_id, bad signature encoding, signature
// mismatch, or schema violation all return an error and no manifest.
func ParseAndVerify(data []byte, kr *Keyring) (*Manifest, error) {
	if kr == nil {
		return nil, fmt.Errorf("release: nil keyring")
	}
	var sm SignedManifest
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sm); err != nil {
		return nil, fmt.Errorf("release: parse releases.json: %w", err)
	}
	if len(sm.Signatures) == 0 {
		return nil, fmt.Errorf("release: no signatures on manifest")
	}
	canonical, err := canonicalBytes(sm.Manifest)
	if err != nil {
		return nil, fmt.Errorf("release: marshal manifest: %w", err)
	}
	var verified bool
	var lastErr error
	for _, s := range sm.Signatures {
		if err := kr.verifyOne(canonical, s); err != nil {
			lastErr = err
			continue
		}
		verified = true
		break
	}
	if !verified {
		if lastErr == nil {
			lastErr = fmt.Errorf("no usable signature")
		}
		return nil, fmt.Errorf("release: manifest signature rejected: %w", lastErr)
	}
	if err := validateManifest(&sm.Manifest); err != nil {
		return nil, err
	}
	return &sm.Manifest, nil
}

// validateManifest enforces the schema invariants on an already
// signature-verified manifest. Belt and suspenders: a valid signature does
// not imply sane fields.
func validateManifest(m *Manifest) error {
	if m.ManifestVersion != currentManifestVersion {
		return fmt.Errorf("release: unsupported manifest_version %d (want %d)", m.ManifestVersion, currentManifestVersion)
	}
	if _, err := time.Parse(time.RFC3339, m.GeneratedAt); err != nil {
		return fmt.Errorf("release: bad generated_at: %w", err)
	}
	if len(m.Channels) == 0 {
		return fmt.Errorf("release: no channels in manifest")
	}
	for name, ch := range m.Channels {
		if name == "" {
			return fmt.Errorf("release: empty channel name")
		}
		if ch.Latest == "" {
			return fmt.Errorf("release: channel %q has no latest", name)
		}
		if !isVersion(ch.Latest) || !isVersion(ch.MinSupportedVersion) {
			return fmt.Errorf("release: channel %q has bad version strings", name)
		}
		if len(ch.Releases) == 0 {
			return fmt.Errorf("release: channel %q has no releases", name)
		}
		for i := range ch.Releases {
			if err := validateRelease(name, &ch.Releases[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateRelease(channel string, r *Release) error {
	if !isVersion(r.Version) {
		return fmt.Errorf("release: channel %q: bad version %q", channel, r.Version)
	}
	if _, err := time.Parse(time.RFC3339, r.ReleasedAt); err != nil {
		return fmt.Errorf("release: channel %q version %s: bad released_at: %w", channel, r.Version, err)
	}
	if r.Notes == "" {
		return fmt.Errorf("release: channel %q version %s: notes must not be empty", channel, r.Version)
	}
	if len(r.Artifacts) == 0 {
		return fmt.Errorf("release: channel %q version %s: no artifacts", channel, r.Version)
	}
	for platform, a := range r.Artifacts {
		parts := strings.Split(platform, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("release: channel %q version %s: bad platform key %q (want GOOS/GOARCH)", channel, r.Version, platform)
		}
		u, err := url.Parse(a.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("release: channel %q version %s: artifact %q must be an https URL", channel, r.Version, platform)
		}
		if len(a.SHA256) != 64 || !isHex(a.SHA256) {
			return fmt.Errorf("release: channel %q version %s: artifact %q sha256 must be 64 hex chars", channel, r.Version, platform)
		}
	}
	return nil
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// isVersion accepts dotted numeric versions (e.g. "0.2.0"). Pre-release
// suffixes are NOT supported: node versions are plain dotted numbers
// stamped from the VERSION file.
func isVersion(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) < 2 || len(parts) > 4 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

// CompareVersions compares two dotted numeric versions.
// Returns -1, 0, +1 for a<b, a==b, a>b. Missing trailing parts are zero
// ("1.2" == "1.2.0"). Invalid inputs sort below valid ones; two invalid
// inputs compare equal.
func CompareVersions(a, b string) int {
	pa, oka := numParts(a)
	pb, okb := numParts(b)
	if !oka || !okb {
		switch {
		case oka:
			return 1
		case okb:
			return -1
		default:
			return 0
		}
	}
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func numParts(v string) ([]int, bool) {
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			return nil, false
		}
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return nil, false
			}
			n = n*10 + int(c-'0')
		}
		out = append(out, n)
	}
	return out, len(out) > 0
}

// LatestRelease returns the release named by the channel's "latest" field,
// or an error if it is missing or yanked. A yanked "latest" is treated as
// no safe release, not as permission to serve the yanked bytes.
func (m *Manifest) LatestRelease(channel string) (*Release, error) {
	ch, ok := m.Channels[channel]
	if !ok {
		return nil, fmt.Errorf("release: unknown channel %q", channel)
	}
	for i := range ch.Releases {
		if ch.Releases[i].Version == ch.Latest {
			if ch.Releases[i].Yanked {
				return nil, fmt.Errorf("release: channel %q latest %s is yanked", channel, ch.Latest)
			}
			return &ch.Releases[i], nil
		}
	}
	return nil, fmt.Errorf("release: channel %q latest %s not in releases", channel, ch.Latest)
}

// Supported reports whether nodeVersion meets the channel's
// min_supported_version. Used by the update check (rung U2) to warn loudly
// on nodes too old to trust.
func (m *Manifest) Supported(channel, nodeVersion string) (bool, error) {
	ch, ok := m.Channels[channel]
	if !ok {
		return false, fmt.Errorf("release: unknown channel %q", channel)
	}
	if !isVersion(nodeVersion) {
		return false, fmt.Errorf("release: bad node version %q", nodeVersion)
	}
	return CompareVersions(nodeVersion, ch.MinSupportedVersion) >= 0, nil
}
