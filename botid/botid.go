// Package botid is the one place that decides how a cl0q crawler node identifies itself on the web: the
// User-Agent every request carries, and the robots.txt user-agent names a node obeys.
//
// Every node sends the same product token, cl0q-node, plus the operator id it was enrolled with:
//
//	Mozilla/5.0 (compatible; cl0q-node/1.0; operator=<id>; +https://cl0q.com/bot)
//
// so a site owner can block every node with one robots.txt group ("User-agent: cl0q-node") and can tell us
// which node to look at if one misbehaves. The User-Agent is deliberately NOT configurable: a node that could
// rename itself could hide from the people it is crawling.
package botid

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

const (
	// Product is the robots.txt / User-Agent token shared by every community node.
	Product = "cl0q-node"
	// Version is the User-Agent version. Bump it only for behaviour changes site owners would care about.
	Version = "1.0"
	// InfoURL explains what the bot is and how to block or contact us.
	InfoURL = "https://cl0q.com/bot"
	// EnvNodeID is the environment variable holding the operator id assigned at enrollment.
	EnvNodeID = "CL0Q_NODE_ID"
)

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,31}$`)

// knownAgents are the robots.txt user-agent tokens that address cl0q nodes.
// Per RFC 9309 §2.2.2, matching is prefix-based: a User-agent line binds any
// crawler whose product token starts with the line value (case-insensitive).
var knownAgents = []string{
	Product, // cl0q-node
	"cl0q-crawler",
	"cl0q-discovery",
	"cl0q-favicon",
	"cl0q-favicon-grabber", // former name of cl0q-favicon
	"cl0q",
}

// UserAgent builds the User-Agent for the given node id (2–32 chars of a-z, 0-9, '-' and '_').
func UserAgent(nodeID string) (string, error) {
	if !idRE.MatchString(nodeID) {
		return "", fmt.Errorf("%s=%q is not a valid node id (2-32 chars: a-z, 0-9, '-', '_')", EnvNodeID, nodeID)
	}
	return fmt.Sprintf("Mozilla/5.0 (compatible; %s/%s; operator=%s; +%s)", Product, Version, nodeID, InfoURL), nil
}

// UserAgentFromEnv reads CL0Q_NODE_ID and returns the User-Agent, or an
// error: a node must never crawl anonymously, but a library must not call
// os.Exit on its host — the caller decides how to handle a missing or
// invalid node id.
func UserAgentFromEnv() (string, error) {
	id := strings.TrimSpace(os.Getenv(EnvNodeID))
	if id == "" {
		return "", fmt.Errorf("%s is not set. Every request a node makes names its operator; use the node id you were given "+
			"when you enrolled (see docs/CRAWLER_NODES.md)", EnvNodeID)
	}
	return UserAgent(id)
}

// MustUserAgent reads CL0Q_NODE_ID and returns the User-Agent, panicking if
// the node id is missing or invalid (standard Must semantics: recoverable,
// unlike os.Exit — the caller can recover if it wants to keep running).
func MustUserAgent() string {
	ua, err := UserAgentFromEnv()
	if err != nil {
		panic(err)
	}
	return ua
}

// IsOurs reports whether a robots.txt User-agent value addresses cl0q nodes.
// Per RFC 9309 §2.2.2, matching is prefix-based: a User-agent line binds any
// crawler whose product token starts with the line value (case-insensitive).
// Besides cl0q-node it accepts the names of cl0q's own bots (cl0q-crawler,
// cl0q-discovery, cl0q-favicon and its former name cl0q-favicon-grabber)
// and plain "cl0q", so a rule a site wrote for any cl0q bot also binds every node.
func IsOurs(agent string) bool {
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent == "" {
		return false
	}
	for _, known := range knownAgents {
		if strings.HasPrefix(known, agent) {
			return true
		}
	}
	return false
}
