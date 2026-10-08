package trust

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"time"
)

// signMsg builds the canonical message that is authenticated:
// nodeID || 0x00 || url || 0x00 || contentHash || unix-seconds (big-endian).
// The 0x00 separators prevent concatenation ambiguity between the
// variable-length fields.
func signMsg(nodeID, url string, contentHash [32]byte, ts time.Time) []byte {
	msg := make([]byte, 0, len(nodeID)+1+len(url)+1+32+8)
	msg = append(msg, nodeID...)
	msg = append(msg, 0x00)
	msg = append(msg, url...)
	msg = append(msg, 0x00)
	msg = append(msg, contentHash[:]...)
	var tsb [8]byte
	binary.BigEndian.PutUint64(tsb[:], uint64(ts.Unix()))
	msg = append(msg, tsb[:]...)
	return msg
}

// SignResult authenticates one crawl result at submit time. The secret is
// the node's refresh_secret (server and node both hold it from enrollment),
// so a signature is only forgeable by the node itself — and any downstream
// tampering with nodeID, URL, content hash, or timestamp invalidates it.
//
// The signature is NOT replay protection across submissions: the timestamp
// binds the signature to second granularity; downstream systems should also
// check the timestamp against a freshness window (see G12-TRUST.md).
func SignResult(secret []byte, nodeID, url string, contentHash [32]byte, ts time.Time) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write(signMsg(nodeID, url, contentHash, ts))
	return mac.Sum(nil)
}

// VerifyResult returns true when sig is a valid signature for the given
// fields under secret. It is constant-time in the MAC comparison.
func VerifyResult(secret, sig []byte, nodeID, url string, contentHash [32]byte, ts time.Time) bool {
	mac := hmac.New(sha256.New, secret)
	mac.Write(signMsg(nodeID, url, contentHash, ts))
	return hmac.Equal(mac.Sum(nil), sig)
}

// ContentHash returns the SHA-256 of a fetched body — the canonical
// "what the page contained" fingerprint the verifier compares.
func ContentHash(body []byte) [32]byte {
	return sha256.Sum256(body)
}
