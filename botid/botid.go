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
	"log"
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

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

// UserAgent builds the User-Agent for the given node id (2–32 chars of a-z, 0-9 and '-').
func UserAgent(nodeID string) (string, error) {
	if !idRE.MatchString(nodeID) {
		return "", fmt.Errorf("%s=%q is not a valid node id (2-32 chars: a-z, 0-9, '-')", EnvNodeID, nodeID)
	}
	return fmt.Sprintf("Mozilla/5.0 (compatible; %s/%s; operator=%s; +%s)", Product, Version, nodeID, InfoURL), nil
}

// MustUserAgent reads CL0Q_NODE_ID and returns the User-Agent, or exits: a node must never crawl anonymously.
func MustUserAgent() string {
	id := strings.TrimSpace(os.Getenv(EnvNodeID))
	if id == "" {
		log.Fatalf("%s is not set. Every request a node makes names its operator; use the node id you were given "+
			"when you enrolled (see docs/CRAWLER_NODES.md).", EnvNodeID)
	}
	ua, err := UserAgent(id)
	if err != nil {
		log.Fatal(err)
	}
	return ua
}

// IsOurs reports whether a robots.txt User-agent value addresses cl0q nodes. Besides cl0q-node it accepts the
// names of cl0q's own bots (cl0q-crawler, cl0q-discovery, cl0q-favicon and its former name cl0q-favicon-grabber)
// and plain "cl0q", so a rule a site wrote for any cl0q bot also binds every node.
func IsOurs(agent string) bool {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case Product, "cl0q-crawler", "cl0q-discovery", "cl0q-favicon", "cl0q-favicon-grabber", "cl0q":
		return true
	}
	return false
}
