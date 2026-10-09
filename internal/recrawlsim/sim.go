// Package recrawlsim implements the HE-4 design-note §8 simulation: a
// synthetic crawl fleet with mixed change-rate pages, comparing recrawl
// scheduling policies on importance-weighted fresh-index lag vs crawl
// budget.
//
// This is a test harness, not production infrastructure. The change process
// is generated once per run (seeded) and replayed identically for every
// policy, so policy differences are the only source of variance.
//
// Modeling fidelity notes (what the sim models vs the real node):
//   - (a) "today": unordered draw + legacy T3 (fixed ×0.75 on any 200,
//     ×1.25 on 304, cap 30d) on 200/304 verdicts. Servers that send no
//     usable validators can never 304, so legacy T3 misreads their
//     unchanged 200s as "changed" and tightens — the misclassification
//     §3.2's SimHash verdict fixes.
//   - (b)-(d): the §4/§5 stack as specified — binary-observation EWMA for
//     change_rate (mirrors he4NewChangeRate in cmd/website-crawler/he4.go),
//     λ-aware T3 factors on the SimHash verdict (modeled as perfect), and
//     the P = importance × expected_gain × overdue draw with the sitemap
//     boost. Per the design's policy ladder, (a)-(c) pay full fetch cost
//     while (d) pays 0.1 for unchanged (304) fetches.
//   - Known simplification: the design claims the T3 interval "converges to
//     ≈1/λ"; the multiplicative dynamics actually converge to ≈0.3/λ (the
//     fixed point where P(change per fetch) ≈ 25%). The sim models the
//     dynamics, not the claim.
package recrawlsim

import (
	"math"
	"math/rand"
	"sort"
)

// Workload mix: fraction of domains in each true change-rate class.
// (design note §8: 1% daily-changers, 10% weekly, rest monthly/yearly)
type mixClass struct {
	frac   float64
	lambda float64 // true changes/day
}

var workloadMix = []mixClass{
	{0.01, 1.0},       // daily changers (news homepages)
	{0.10, 1.0 / 7},   // weekly
	{0.40, 1.0 / 30},  // monthly
	{0.49, 1.0 / 365}, // yearly (brochure sites)
}

const (
	priorLambda        = 1.0 / 120 // §4 default change-rate prior (changes/day)
	ewmaAlpha          = 0.3       // §4 EWMA learning rate for change_rate
	lambdaMin          = 1.0 / 365
	lambdaMax          = 2.0
	recrawlDefaultDays = 120.0 // RECRAWL_DAYS: initial interval (NULL recrawl_interval_days)
	intervalFloor      = 1.0   // T3 floor (days)
	intervalCap        = 30.0  // T3 cap (days)
	fetchCost          = 1.0
	notModCost         = 0.1  // 304: zero content cost (design note §8)
	sitemapBoost       = 2.0  // matches he4SitemapBoost in prod
	sitemapTrust       = 0.70 // fraction of domains with a reliable sitemap lastmod
	validatorFrac      = 0.70 // fraction of domains whose servers send usable validators
)

// Policy is one of the four scheduling stacks compared in §8.
type Policy int

const (
	// PolicyUniform is (a): today's unordered LIMIT draw + legacy T3 —
	// no learning, no prioritization, full fetch cost always.
	PolicyUniform Policy = iota
	// PolicyPriority is (b): §5 priority draw P = importance ×
	// expected_gain × overdue, with the §4 change_rate EWMA learned from
	// SimHash verdicts and λ-aware T3 intervals.
	PolicyPriority
	// PolicySitemap is (c): (b) + sitemap-lastmod pre-fetch promotion.
	PolicySitemap
	// PolicyFull is (d): (c) + conditional-GET cost model (304s cost 0.1).
	PolicyFull
)

func (p Policy) String() string {
	switch p {
	case PolicyUniform:
		return "uniform"
	case PolicyPriority:
		return "priority"
	case PolicySitemap:
		return "priority+sitemap"
	case PolicyFull:
		return "priority+sitemap+condget"
	}
	return "unknown"
}

// domain is one synthetic crawled page (domain-granular, per the survey:
// website-crawler recrawls homepages, not URLs).
type domain struct {
	importance    float64 // Zipf: 1/rank
	lambda        float64 // true changes/day (hidden from the scheduler)
	changes       []int   // sorted days on which the page truly changed
	sitemapOK     bool    // sitemap lastmod is a reliable staleness hint
	hasValidators bool    // server sends ETag/Last-Modified → 304 possible
}

// workload is a fixed synthetic web, generated once and replayed per policy.
type workload struct {
	domains   []domain
	days      int
	initCrawl []int   // staggered initial lastCrawl per domain
	impSum    float64 // Σ importance (normalizer for the lag metric)
	decile    []int   // importance decile per domain (0 = top)
}

// generate builds the synthetic web: Zipf importance, mixed Poisson change
// processes, 70% reliable sitemap lastmods, 70% validator support.
// Deterministic for a seed.
func generate(nDomains, days int, seed int64) *workload {
	rng := rand.New(rand.NewSource(seed))
	w := &workload{days: days, initCrawl: make([]int, nDomains), decile: make([]int, nDomains)}

	// Assign change-rate classes by shuffling domain indices.
	order := rng.Perm(nDomains)
	classes := make([]mixClass, nDomains)
	pos := 0
	for _, mc := range workloadMix {
		k := int(mc.frac * float64(nDomains))
		for i := 0; i < k && pos < nDomains; i++ {
			classes[order[pos]] = mc
			pos++
		}
	}
	for ; pos < nDomains; pos++ { // rounding remainder -> slowest class
		classes[order[pos]] = workloadMix[len(workloadMix)-1]
	}

	w.domains = make([]domain, nDomains)
	for i := range w.domains {
		lam := classes[i].lambda
		// Poisson change days via exponential inter-arrivals.
		var ch []int
		for t := 0.0; ; {
			t += -math.Log(1-rng.Float64()) / lam
			day := int(math.Ceil(t))
			if day >= days {
				break
			}
			ch = append(ch, day)
		}
		w.domains[i] = domain{
			importance:    1.0 / float64(i+1), // rank i+1, Zipf
			lambda:        lam,
			changes:       ch,
			sitemapOK:     rng.Float64() < sitemapTrust,
			hasValidators: rng.Float64() < validatorFrac,
		}
		w.impSum += w.domains[i].importance
		w.initCrawl[i] = -rng.Intn(int(recrawlDefaultDays)) // staggered start
	}
	// Deciles by importance rank (0 = top decile). Importance is 1/(i+1) so
	// rank order == index order.
	for i := range w.decile {
		w.decile[i] = i * 10 / nDomains
	}
	return w
}

// lastChangeDay returns the most recent true change day <= day, or -1.
func (d *domain) lastChangeDay(day int) int {
	i := sort.Search(len(d.changes), func(i int) bool { return d.changes[i] > day }) - 1
	if i < 0 {
		return -1
	}
	return d.changes[i]
}

// hinted reports whether the sitemap lastmod looks newer than our copy.
// Unreliable sitemaps stamp lastmod=now() on every build (design note §9.4),
// so they always look fresh — pure noise the scheduler must not trust alone.
func (d *domain) hinted(day, lastCrawl int) bool {
	if d.sitemapOK {
		return d.lastChangeDay(day) > lastCrawl
	}
	return true
}

func clamp(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

// Config tunes one simulation run.
type Config struct {
	Budget float64 // fetch-cost units available per day
	Seed   int64   // shuffle seed for the uniform policy
	Warmup int     // days before metrics start accumulating (state still evolves)
}

// Metrics collected over a run.
type Metrics struct {
	LagDays    float64             // mean importance-weighted staleness, in days (§8)
	LagByClass map[float64]float64 // ... per true-λ class (diagnostic)
	Fetches    int                 // total fetches issued
	SpentTotal float64             // total budget units spent
	Spent304   float64             // budget units spent on unchanged (304) fetches
	MaxOverdue [10]float64         // max age/interval per importance decile (starvation check)
	measured   int                 // days the lag metric accumulated over
}

// Pct304 returns the share of budget spent on unchanged fetches.
func (m Metrics) Pct304() float64 {
	if m.SpentTotal == 0 {
		return 0
	}
	return 100 * m.Spent304 / m.SpentTotal
}

type schedState struct {
	lamHat    float64 // change_rate estimate: binary-observation EWMA (§4)
	interval  float64 // T3 adaptive recrawl interval, days
	lastCrawl int
}

func newSchedState() schedState {
	return schedState{lamHat: priorLambda, interval: recrawlDefaultDays, lastCrawl: 0}
}

// learn applies one binary fetch verdict to the change_rate EWMA.
// Mirrors he4NewChangeRate in cmd/website-crawler/he4.go: the estimator the
// design specifies (not the interval-censored Poisson MLE, which would be
// better calibrated — filed as a follow-up on #146).
func (s *schedState) learn(changed bool) {
	obs := 0.0
	if changed {
		obs = 1.0
	}
	s.lamHat = clamp((1-ewmaAlpha)*s.lamHat+ewmaAlpha*obs, lambdaMin, lambdaMax)
}

// adaptT3 applies the multiplicative interval update. legacy=true reproduces
// today's T3 (fixed ×0.75 on any 200, ×1.25 on 304); otherwise the HE-4
// λ-aware factors driven by the SimHash verdict (mirrors the SQL in
// he4UpdateSignals / t3NextIntervalLambda).
func (s *schedState) adaptT3(legacy bool, changed bool) {
	if legacy {
		if changed {
			s.interval *= 0.75
		} else {
			s.interval *= 1.25
		}
	} else {
		if changed {
			s.interval *= 0.5 + 0.25*math.Exp(-s.lamHat)
		} else {
			s.interval *= 1.1 + 0.15*(1-math.Exp(-s.lamHat))
		}
	}
	s.interval = clamp(s.interval, intervalFloor, intervalCap)
}

// priorityScore renders he4PriorityScore for the sim's per-domain state.
func (s *schedState) priorityScore(importance float64, age int, hinted bool) float64 {
	lam := s.lamHat
	if lam <= 0 {
		lam = priorLambda
	}
	ageDays := float64(age)
	gain := 1 - math.Exp(-lam*ageDays)
	overdue := ageDays / s.interval
	p := importance * gain * overdue
	if hinted {
		p *= sitemapBoost
	}
	return p
}

// Run executes one policy against the workload and returns its metrics.
func Run(w *workload, pol Policy, cfg Config) Metrics {
	n := len(w.domains)
	st := make([]schedState, n)
	for i := range st {
		st[i] = newSchedState()
		st[i].lastCrawl = w.initCrawl[i]
	}
	var m Metrics
	m.LagByClass = map[float64]float64{}
	rng := rand.New(rand.NewSource(cfg.Seed))

	type cand struct {
		idx int
		p   float64
	}
	due := make([]cand, 0, n)

	for day := 0; day < w.days; day++ {
		// 1. Draw the recrawl-due set and score it.
		due = due[:0]
		for i, d := range w.domains {
			s := &st[i]
			age := day - s.lastCrawl
			if float64(age) < s.interval {
				continue // not due
			}
			p := 0.0
			if pol != PolicyUniform {
				p = s.priorityScore(d.importance, age,
					(pol == PolicySitemap || pol == PolicyFull) && d.hinted(day, s.lastCrawl))
			}
			due = append(due, cand{idx: i, p: p})
		}
		if pol == PolicyUniform {
			rng.Shuffle(len(due), func(a, b int) { due[a], due[b] = due[b], due[a] })
		} else {
			sort.Slice(due, func(a, b int) bool { return due[a].p > due[b].p })
		}

		// 2. Spend the daily budget in draw order.
		remaining := cfg.Budget
		for _, c := range due {
			i := c.idx
			d := &w.domains[i]
			s := &st[i]
			changed := d.lastChangeDay(day) > s.lastCrawl
			cost := fetchCost
			if pol == PolicyFull && !changed {
				cost = notModCost // conditional GET: 304, zero content cost
			}
			if cost > remaining {
				break
			}
			remaining -= cost
			m.Fetches++
			m.SpentTotal += cost
			if !changed {
				m.Spent304 += cost
			}
			// 3. Learn from the fetch verdict and adapt the interval.
			if pol == PolicyUniform {
				// Legacy T3 sees 200/304, not truth: an unchanged page
				// without validators returns 200 and mis-tightens.
				s.adaptT3(true, changed || !d.hasValidators)
			} else {
				// HE-4: SimHash verdict (modeled as truth) drives both the
				// EWMA and the λ-aware T3 factors.
				s.learn(changed)
				s.adaptT3(false, changed)
			}
			s.lastCrawl = day
		}

		// 4. Accumulate importance-weighted staleness (§8 lag metric),
		// after the warmup period.
		if day >= cfg.Warmup {
			m.measured++
			for i, d := range w.domains {
				lc := d.lastChangeDay(day)
				if lc > st[i].lastCrawl {
					stale := d.importance * float64(day-lc)
					m.LagDays += stale
					m.LagByClass[d.lambda] += stale
				}
			}
		}
	}

	if m.measured > 0 {
		m.LagDays /= float64(m.measured) * w.impSum
		for lam, v := range m.LagByClass {
			m.LagByClass[lam] = v / float64(m.measured) / w.impSum
		}
	}

	// 5. Starvation check: max overdue per importance decile at end of run.
	for i := range w.domains {
		s := &st[i]
		od := float64(w.days-s.lastCrawl) / s.interval
		dec := w.decile[i]
		if od > m.MaxOverdue[dec] {
			m.MaxOverdue[dec] = od
		}
	}
	return m
}
