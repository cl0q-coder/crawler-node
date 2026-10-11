// Pinned release-signing keys and the keyring.
//
// KEY CUSTODY IS AN OPEN DECISION (Chris, 2026-10-11): where the private
// release-signing key lives and who may sign a release has NOT been decided.
// Until it is, this file pins TEST KEYS ONLY. They are named Test* and
// documented as test fixtures throughout. NEVER treat these as production
// keys, and never sign a real release with the matching test private keys
// (kept in release_test.go fixtures).
//
// Going production is a one-line change: build the real Keyring from the
// real public keys and assign it to DefaultKeyring (or pass it explicitly
// to ParseAndVerify). The two-key mechanism below is how rotation works
// without bricking old nodes:
//
//  1. Ship TWO pinned public keys from day one (A and B).
//  2. Sign every manifest with ONE of them (key_id says which).
//  3. To rotate: generate key C, add its public half to the keyring,
//     start signing with C. Nodes built before the rotation still verify
//     because A and B remain pinned.
//  4. To retire a compromised key: remove it from the keyring in a new
//     node release. Nodes that never upgrade keep trusting the old key —
//     that is why compromise response also means yanking releases and
//     shipping the new node build fast (Track U rung U7 covers rollout).
//
// Generating a production keypair (run once, offline, when Chris decides):
//
//	priv := ed25519.GenerateKey(rand.Reader) // keep the seed OFFLINE
//	pub  := priv.Public().(ed25519.PublicKey)
//	fmt.Println(base64.StdEncoding.EncodeToString(pub)) // embed below
package release

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
)

// PinnedKey is one release-signing public key the node trusts.
type PinnedKey struct {
	// ID is the key_id string that appears in manifest signatures.
	ID string
	// PubKey is the 32-byte Ed25519 public key.
	PubKey ed25519.PublicKey
}

// Keyring is the set of pinned public keys a node verifies manifests
// against. A manifest is accepted if ANY pinned key has a valid signature.
type Keyring struct {
	keys map[string]ed25519.PublicKey
}

// NewKeyring builds a keyring from pinned keys. Duplicate IDs are rejected.
func NewKeyring(keys ...PinnedKey) (*Keyring, error) {
	kr := &Keyring{keys: make(map[string]ed25519.PublicKey, len(keys))}
	for _, k := range keys {
		if k.ID == "" {
			return nil, fmt.Errorf("release: pinned key with empty ID")
		}
		if len(k.PubKey) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("release: pinned key %q has bad public key length %d", k.ID, len(k.PubKey))
		}
		if _, dup := kr.keys[k.ID]; dup {
			return nil, fmt.Errorf("release: duplicate pinned key ID %q", k.ID)
		}
		// Copy: the caller must not be able to mutate the pinned key later.
		cp := make(ed25519.PublicKey, ed25519.PublicKeySize)
		copy(cp, k.PubKey)
		kr.keys[k.ID] = cp
	}
	if len(kr.keys) == 0 {
		return nil, fmt.Errorf("release: keyring must pin at least one key")
	}
	return kr, nil
}

// verifyOne checks a single signature against the pinned key it names.
func (kr *Keyring) verifyOne(canonical []byte, s ManifestSignature) error {
	pub, ok := kr.keys[s.KeyID]
	if !ok {
		return fmt.Errorf("unknown key_id %q", s.KeyID)
	}
	raw, err := base64.StdEncoding.DecodeString(s.Signature)
	if err != nil {
		return fmt.Errorf("key_id %q: bad signature encoding: %w", s.KeyID, err)
	}
	if len(raw) != ed25519.SignatureSize {
		return fmt.Errorf("key_id %q: bad signature length %d", s.KeyID, len(raw))
	}
	if !ed25519.Verify(pub, canonical, raw) {
		return fmt.Errorf("key_id %q: signature mismatch", s.KeyID)
	}
	return nil
}

// mustDecodeTestKey decodes a base64 test public key. It panics on bad
// input: test keys are compile-time constants, so a bad one is a
// programmer error, not a runtime condition.
func mustDecodeTestKey(s string) ed25519.PublicKey {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		panic("release: bad test public key constant")
	}
	return ed25519.PublicKey(raw)
}

// TestPinnedKeys are the two pinned TEST public keys. See the file header:
// these are fixtures, not production key material.
var TestPinnedKeys = []PinnedKey{
	{
		ID:     "test-key-a",
		PubKey: mustDecodeTestKey("EATLxLFaocUYD5YstRUL+ZWwOmQZmWydojWJifsWSns="),
	},
	{
		ID:     "test-key-b",
		PubKey: mustDecodeTestKey("JLjjyPJQdF+0OYkmKIY5EgvA9v4srCCB2A4ypPjtzkU="),
	},
}

// DefaultKeyring is what ParseAndVerify uses when the caller does not pass
// a keyring explicitly. TODAY it pins the TEST keys. When Chris decides
// key custody, replace this with NewKeyring(realKeyA, realKeyB).
var DefaultKeyring, _ = NewKeyring(TestPinnedKeys...)
