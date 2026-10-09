package trust

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/bits"
)

// Sybil resistance: proof-of-work at enrollment.
//
// Spinning up 1000 fake nodes must cost real compute, otherwise a Sybil
// fleet can out-vote honest nodes and launder poisoned results through the
// verification sample. The mechanism is Hashcash-style: the server issues a
// challenge (seed + difficulty); the client finds a nonce such that
// SHA-256(seed || nodeID || nonce) has at least Difficulty leading zero
// bits. Verification is one hash — cheap for the server, expensive to
// parallelize per fake node for the attacker.
//
// Difficulty guidance (leading zero bits):
//   - 16-20: seconds of CPU on a laptop — casual Sybil deterrent.
//   - 22-24: minutes — serious fleet onboarding friction.
// The server picks the difficulty; the client just solves. The tradeoff
// analysis (PoW vs invite tiers vs stake) lives in G12-TRUST.md.

// Challenge is issued by the server during enrollment.
type Challenge struct {
	Seed       string `json:"seed"`       // server-random, unique per enrollment attempt
	Difficulty uint   `json:"difficulty"` // required leading zero bits, 1..32
}

// ErrTooHard is returned when SolveChallenge cannot find a nonce within the
// iteration budget — the difficulty is set absurdly high.
var ErrTooHard = errors.New("trust: proof-of-work difficulty exceeds iteration budget")

// maxIterations bounds a solve attempt. At difficulty 24 a solution takes on
// average 2^23 hashes (~seconds on modern CPUs); the budget caps runaway work
// if the server misconfigures difficulty.
const maxIterations = 1 << 28

// powDigest is SHA-256(seed || 0x00 || nodeID || 0x00 || nonce big-endian).
func powDigest(seed, nodeID string, nonce uint64) [32]byte {
	buf := make([]byte, 0, len(seed)+1+len(nodeID)+1+8)
	buf = append(buf, seed...)
	buf = append(buf, 0x00)
	buf = append(buf, nodeID...)
	buf = append(buf, 0x00)
	var nb [8]byte
	binary.BigEndian.PutUint64(nb[:], nonce)
	buf = append(buf, nb[:]...)
	return sha256.Sum256(buf)
}

// leadingZeroBits counts the leading zero bits of a digest.
func leadingZeroBits(d [32]byte) uint {
	var n uint
	for _, b := range d {
		z := uint(bits.LeadingZeros8(b))
		n += z
		if z < 8 {
			break
		}
	}
	return n
}

// SolveChallenge finds a nonce satisfying the challenge for nodeID.
func SolveChallenge(ch Challenge, nodeID string) (uint64, error) {
	if ch.Difficulty < 1 || ch.Difficulty > 32 {
		return 0, errors.New("trust: difficulty must be in 1..32")
	}
	if ch.Seed == "" || nodeID == "" {
		return 0, errors.New("trust: seed and nodeID must be non-empty")
	}
	for nonce := uint64(0); nonce < maxIterations; nonce++ {
		if leadingZeroBits(powDigest(ch.Seed, nodeID, nonce)) >= ch.Difficulty {
			return nonce, nil
		}
	}
	return 0, ErrTooHard
}

// VerifySolution checks a claimed nonce against the challenge. One hash:
// cheap enough to run on every enrollment.
func VerifySolution(ch Challenge, nodeID string, nonce uint64) bool {
	if ch.Difficulty < 1 || ch.Difficulty > 32 || ch.Seed == "" || nodeID == "" {
		return false
	}
	return leadingZeroBits(powDigest(ch.Seed, nodeID, nonce)) >= ch.Difficulty
}
