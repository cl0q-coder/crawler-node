package trust

import (
	"sync"
	"time"
)

// Trust tiers. A node's tier decides which work it receives; see RouteWork.
type Tier int

const (
	// TierFull receives the full work queue.
	TierFull Tier = iota
	// TierRestricted receives low-value work only (probe reserve, low-priority recrawls).
	TierRestricted
	// TierQuarantined receives no work until a verifier re-promotes it.
	TierQuarantined
)

func (t Tier) String() string {
	switch t {
	case TierFull:
		return "full"
	case TierRestricted:
		return "restricted"
	case TierQuarantined:
		return "quarantined"
	default:
		return "unknown"
	}
}

// Scoring parameters. Tunables, not constants — a server deploy can tighten
// or loosen them without a client release.
const (
	// Tier boundaries on the combined score.
	TierFullMin       = 0.80
	TierRestrictedMin = 0.50

	// ProvisionalScore is where every new node starts. Nodes earn trust;
	// they do not start with it. Provisional nodes can never reach TierFull.
	ProvisionalScore = 0.60
	// MinOutcomesBeforeFull is how many verification outcomes a node needs
	// before it can leave provisional status.
	MinOutcomesBeforeFull = 25

	// EWMA alpha for the verification signal: recent outcomes matter most.
	verifAlpha = 0.15
)

// Weights for the three trust signals. Verification dominates because it is
// the only signal an adversary cannot fake cheaply.
const (
	weightVerif   = 0.60
	weightUptime  = 0.25
	weightQuality = 0.15
)

type nodeState struct {
	nodeID         string
	verifScore     float64 // EWMA of verification matches, init ProvisionalScore
	outcomes       int     // total verification outcomes recorded
	heartbeatsOK   int
	heartbeatsTot  int
	qualityBad     int
	qualityTot     int
	provisional    bool
	quarantineHold bool // set by operator; only verifier re-promotion clears it
	lastSeen       time.Time
}

// TrustScorer tracks per-node trust from verification history, heartbeat
// uptime, and data quality. Safe for concurrent use.
type TrustScorer struct {
	mu    sync.Mutex
	nodes map[string]*nodeState
}

// NewTrustScorer returns an empty scorer.
func NewTrustScorer() *TrustScorer {
	return &TrustScorer{nodes: make(map[string]*nodeState)}
}

func (s *TrustScorer) nodeLocked(nodeID string) *nodeState {
	st, ok := s.nodes[nodeID]
	if !ok {
		st = &nodeState{
			nodeID:      nodeID,
			verifScore:  ProvisionalScore,
			provisional: true,
		}
		s.nodes[nodeID] = st
	}
	return st
}

// RecordVerification folds one verifier outcome into the node. match=true
// means the trusted re-crawl agreed with the node's submitted content.
func (s *TrustScorer) RecordVerification(nodeID string, match bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.nodeLocked(nodeID)
	v := 0.0
	if match {
		v = 1.0
	}
	st.verifScore += verifAlpha * (v - st.verifScore)
	st.outcomes++
	st.lastSeen = time.Now()
	if st.provisional && st.outcomes >= MinOutcomesBeforeFull {
		st.provisional = false
	}
}

// RecordHeartbeat folds one heartbeat into the uptime signal.
func (s *TrustScorer) RecordHeartbeat(nodeID string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.nodeLocked(nodeID)
	st.heartbeatsTot++
	if ok {
		st.heartbeatsOK++
	}
	st.lastSeen = time.Now()
}

// RecordQuality folds one data-quality observation (e.g. one ingest batch)
// into the quality signal. bad/total is the fraction of rows the batch
// carried that were corrupt, unparseable, or rejected by validation.
func (s *TrustScorer) RecordQuality(nodeID string, bad, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.nodeLocked(nodeID)
	st.qualityBad += bad
	st.qualityTot += total
	st.lastSeen = time.Now()
}

// SetQuarantineHold pins a node in quarantine (operator action). Only
// ClearQuarantineHold — e.g. after a verifier re-promotion — releases it.
func (s *TrustScorer) SetQuarantineHold(nodeID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodeLocked(nodeID).quarantineHold = true
}

// ClearQuarantineHold releases an operator quarantine hold.
func (s *TrustScorer) ClearQuarantineHold(nodeID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodeLocked(nodeID).quarantineHold = false
}

func uptimeScore(st *nodeState) float64 {
	if st.heartbeatsTot == 0 {
		return 1.0 // no data yet: do not punish
	}
	return float64(st.heartbeatsOK) / float64(st.heartbeatsTot)
}

func qualityScore(st *nodeState) float64 {
	if st.qualityTot == 0 {
		return 1.0 // no data yet: do not punish
	}
	return 1.0 - float64(st.qualityBad)/float64(st.qualityTot)
}

// Score returns the combined trust score in [0,1].
func (s *TrustScorer) Score(nodeID string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.nodeLocked(nodeID)
	sc := weightVerif*st.verifScore + weightUptime*uptimeScore(st) + weightQuality*qualityScore(st)
	if sc < 0 {
		sc = 0
	}
	if sc > 1 {
		sc = 1
	}
	return sc
}

// Outcomes returns how many verification outcomes a node has on record.
func (s *TrustScorer) Outcomes(nodeID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nodeLocked(nodeID).outcomes
}

// Tier maps the current score to a trust tier. Provisional nodes (fewer
// than MinOutcomesBeforeFull outcomes) can never reach TierFull, no matter
// the score: trust is earned, not assumed. A quarantine hold pins the node
// at TierQuarantined regardless of score.
func (s *TrustScorer) Tier(nodeID string) Tier {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.nodeLocked(nodeID)
	if st.quarantineHold {
		return TierQuarantined
	}
	sc := weightVerif*st.verifScore + weightUptime*uptimeScore(st) + weightQuality*qualityScore(st)
	switch {
	case sc >= TierFullMin && !st.provisional:
		return TierFull
	case sc >= TierRestrictedMin:
		return TierRestricted
	default:
		return TierQuarantined
	}
}

// RouteWork maps a tier to the work class a node may receive. This is the
// enforcement point: low-trust nodes get low-value work (or none), so a
// compromised fleet can do bounded damage while verification catches up.
func RouteWork(t Tier) string {
	switch t {
	case TierFull:
		return "standard"
	case TierRestricted:
		return "low-value"
	default:
		return "none"
	}
}
