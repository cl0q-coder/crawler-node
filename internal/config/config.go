// Package config provides strict startup validation for the crawler-node
// binaries (NR2: config validation + polite defaults).
//
// Rule: every knob is validated at startup. Unparsable input and
// out-of-range values are FATAL errors with a clear message — never a
// silent clamp, never a silent fall back to the default. A bad value must
// refuse to start, not quietly misbehave.
//
// The Validate* functions are pure and unit-tested; the Get*/Check*
// wrappers call log.Fatal on the same conditions the validators detect.
package config

import (
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"codeberg.org/cl0qsearch/crawler-node/botid"
)

// ValidateIntRange reports whether v is inside [lo, hi].
func ValidateIntRange(name string, v, lo, hi int) error {
	if v < lo || v > hi {
		return fmt.Errorf("invalid %s=%d: must be between %d and %d", name, v, lo, hi)
	}
	return nil
}

// ValidateDurationRange reports whether v is inside [lo, hi].
func ValidateDurationRange(name string, v, lo, hi time.Duration) error {
	if v < lo || v > hi {
		return fmt.Errorf("invalid %s=%s: must be between %s and %s", name, v, lo, hi)
	}
	return nil
}

// ValidateNonEmpty reports whether v is non-empty.
func ValidateNonEmpty(name, v string) error {
	if v == "" {
		return fmt.Errorf("invalid %s: must not be empty", name)
	}
	return nil
}

// CheckIntRange fatals unless v is inside [lo, hi]; returns v.
func CheckIntRange(name string, v, lo, hi int) int {
	if err := ValidateIntRange(name, v, lo, hi); err != nil {
		log.Fatal(err)
	}
	return v
}

// CheckDurationRange fatals unless v is inside [lo, hi]; returns v.
func CheckDurationRange(name string, v, lo, hi time.Duration) time.Duration {
	if err := ValidateDurationRange(name, v, lo, hi); err != nil {
		log.Fatal(err)
	}
	return v
}

// CheckNonEmpty fatals on empty v; returns v.
func CheckNonEmpty(name, v string) string {
	if err := ValidateNonEmpty(name, v); err != nil {
		log.Fatal(err)
	}
	return v
}

// GetInt reads an integer env var. Unset -> def. Unparsable -> fatal
// (the old envInt silently fell back to the default, hiding typos like
// WORKERS=lots).
func GetInt(name string, def int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Fatalf("invalid %s=%q: not an integer: %v", name, raw, err)
	}
	return n
}

// GetDurationMs reads a millisecond env var as a time.Duration.
// Unset -> def. Unparsable -> fatal.
func GetDurationMs(name string, def time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Fatalf("invalid %s=%q: not an integer number of milliseconds: %v", name, raw, err)
	}
	return time.Duration(n) * time.Millisecond
}

// GetString reads a string env var. Unset -> def.
func GetString(name, def string) string {
	if raw := os.Getenv(name); raw != "" {
		return raw
	}
	return def
}

// GetBool reads a bool env var. Unset -> def. Unparsable -> fatal.
func GetBool(name string, def bool) bool {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	switch raw {
	case "1", "true", "TRUE", "True", "yes", "YES":
		return true
	case "0", "false", "FALSE", "False", "no", "NO":
		return false
	default:
		log.Fatalf("invalid %s=%q: not a boolean (use 1/0 or true/false)", name, raw)
		return false
	}
}

// SetMemoryLimitDefault caps the Go runtime heap at n bytes unless the
// operator set GOMEMLIMIT explicitly (explicit operator choice wins).
// Call once at startup so a naive configuration cannot OOM the box.
func SetMemoryLimitDefault(n int64) {
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	debug.SetMemoryLimit(n)
}

// nodeIDPrefixLen is how many leading chars of the node id go into the
// public User-Agent (PROTOCOL.md §4). The full node id never appears in a
// User-Agent: node_id + refresh_secret authenticates token rotation, so the
// id is credential-adjacent and must not leak to site operators via their
// access logs. A harvested 8-char prefix is useless for rotation.
const nodeIDPrefixLen = 8

// PublicUserAgent returns the crawl User-Agent for the node named by
// CL0Q_NODE_ID, carrying only the leading nodeIDPrefixLen chars of the id.
// It fatals when CL0Q_NODE_ID is unset or malformed — a node must never
// crawl anonymously or with a forged identity.
func PublicUserAgent() string {
	ua, err := PublicUserAgentFor(nodeIDFromEnv())
	if err != nil {
		log.Fatal(err)
	}
	return ua
}

// PublicUserAgentFor builds the public User-Agent for the given node id,
// truncated to the public prefix. Pure and unit-tested; PublicUserAgent
// adds the env lookup and the fatal-on-missing behavior.
func PublicUserAgentFor(nodeID string) (string, error) {
	if nodeID == "" {
		return "", fmt.Errorf("empty node id")
	}
	prefix := nodeID
	if len(prefix) > nodeIDPrefixLen {
		prefix = prefix[:nodeIDPrefixLen]
	}
	ua, err := botid.UserAgent(prefix)
	if err != nil {
		return "", err
	}
	return ua, nil
}

// nodeIDFromEnv returns CL0Q_NODE_ID or fatals with the same message the
// binaries historically got from botid.MustUserAgent.
func nodeIDFromEnv() string {
	id := strings.TrimSpace(os.Getenv(botid.EnvNodeID))
	if id == "" {
		log.Fatalf("%s is not set. Every request a node makes names its operator; use the node id you were given "+
			"when you enrolled (see docs/CRAWLER_NODES.md).", botid.EnvNodeID)
	}
	return id
}
