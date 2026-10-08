// Package trust implements G-12: node trust + result verification.
//
// Volunteer nodes are untrusted by definition. This package provides the
// client-side primitives and the trust engine:
//
//   - integrity: HMAC-SHA256 content signing at crawl/submit time so
//     tampering is detectable downstream.
//   - verifier: randomly re-crawls a sample of a node's work from a
//     trusted verifier and compares content hashes; mismatch rate feeds
//     the trust score.
//   - score: per-node trust score from verification history, heartbeat
//     uptime, and data quality; tiers gate what work a node receives.
//   - sybil: Hashcash-style proof-of-work for enrollment, so spinning up
//     1000 fake nodes costs real compute.
//
// The server-side half (challenge issuance, the verifier worker loop, the
// admin quarantine view) is specified in G12-TRUST.md; this package is the
// engine those pieces build on.
package trust
