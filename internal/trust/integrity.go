package trust

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"time"
)

// signingKeyDomain separates the signature key from every other use of the
// refresh secret (auth, rotation). Changing it re-keys every node.
const signingKeyDomain = "cl0q-trust-sign/v1"

// DeriveSigningKey derives the HMAC signing key from a node's
// refresh_secret: SHA-256(domain || 0x00 || refresh_secret).
//
// KEY CONTRACT (decision #239, option (a)):
//
// The node derives its signing key locally via DeriveSigningKey applied
// to the refresh_secret it received at enrollment, and signs submissions
// with SignResult. The SERVER provisions the same key at enrollment: when
// it sees the plaintext refresh_secret, it computes
// DeriveSigningKey(refresh_secret) and stores the 32-byte result in the
// node's signing_key column (alongside refresh_secret_HASH). KeyFor reads
// that column. The server NEVER needs the plaintext refresh_secret after
// enrollment, and secret rotation re-derives and updates the stored key,
// so rotation re-keys signatures automatically.
//
// Fail-closed: KeyFor reports whether a key is provisioned for the node.
// A provisioned node with a missing or invalid signature is REJECTED —
// the check never fails open. Only legacy nodes with no key on file skip
// the signature check, and even then SigOK stays false so a skipped check
// is never mistaken for an enforced signature.
func DeriveSigningKey(refreshSecret string) []byte {
	h := sha256.New()
	h.Write([]byte(signingKeyDomain))
	h.Write([]byte{0x00})
	h.Write([]byte(refreshSecret))
	return h.Sum(nil)
}

// signMsg builds the canonical message that is authenticated:
// nodeID || 0x00 || url || 0x00 || contentHash || simHash || unix-seconds
// (big-endian). The 0x00 separators prevent concatenation ambiguity between
// the variable-length fields. simHash is covered so a node cannot claim one
// fingerprint for the hash and submit another.
func signMsg(nodeID, url string, contentHash [32]byte, simHash uint64, ts time.Time) []byte {
	msg := make([]byte, 0, len(nodeID)+1+len(url)+1+32+8+8)
	msg = append(msg, nodeID...)
	msg = append(msg, 0x00)
	msg = append(msg, url...)
	msg = append(msg, 0x00)
	msg = append(msg, contentHash[:]...)
	var shb [8]byte
	binary.BigEndian.PutUint64(shb[:], simHash)
	msg = append(msg, shb[:]...)
	var tsb [8]byte
	binary.BigEndian.PutUint64(tsb[:], uint64(ts.Unix()))
	msg = append(msg, tsb[:]...)
	return msg
}

// SignResult authenticates one crawl result at submit time. secret is the
// node's SIGNING key — DeriveSigningKey(refresh_secret) — not the raw
// refresh_secret. A signature is only forgeable by the node itself, and any
// downstream tampering with nodeID, URL, content hash, simhash, or timestamp
// invalidates it.
//
// The signature is NOT replay protection across submissions: the timestamp
// binds the signature to second granularity; downstream systems should also
// check the timestamp against a freshness window (see G12-TRUST.md).
func SignResult(secret []byte, nodeID, url string, contentHash [32]byte, simHash uint64, ts time.Time) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write(signMsg(nodeID, url, contentHash, simHash, ts))
	return mac.Sum(nil)
}

// VerifyResult returns true when sig is a valid signature for the given
// fields under secret. It is constant-time in the MAC comparison.
func VerifyResult(secret, sig []byte, nodeID, url string, contentHash [32]byte, simHash uint64, ts time.Time) bool {
	mac := hmac.New(sha256.New, secret)
	mac.Write(signMsg(nodeID, url, contentHash, simHash, ts))
	return hmac.Equal(mac.Sum(nil), sig)
}

// ContentHash returns the SHA-256 of a fetched body — the canonical
// "what the page contained" identity fingerprint. Exact identity only;
// the verifier compares CONTENT via SimHash (see verifier.go), not this.
func ContentHash(body []byte) [32]byte {
	return sha256.Sum256(body)
}
