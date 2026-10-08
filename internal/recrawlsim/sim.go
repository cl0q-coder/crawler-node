// Package recrawlsim implements the HE-4 design-note §8 simulation: a
// synthetic crawl fleet with mixed change-rate pages, comparing recrawl
// scheduling policies on importance-weighted fresh-index lag vs crawl
// budget.
//
// This is a test harness, not production infrastructure. The change process
// is generated once per run (seeded) and replayed identically for every
// policy, so policy differences are the only source of variance.
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
	priorLambda  = 1.0 / 120 // §4 default change-rate prior (changes/day)
	ewmaAlpha    = 0.3       // §4 EWMA learning rate for change_rate
	lambdaMin    = 1.0 / 365
	lambdaMax    = 2.0
	intervalMin  = 1.0   // days (T3 floor)
	intervalMax  = 120.0 // days (RECRAWL_DAYS global floor)
	fetchCost    = 1.0
	notModCost   = 0.1  // 304: zero content cost (design note §8)
	sitemapBoost = 3.0  // pre-fetch promotion multiplier for a fresh lastmod hint
	sitemapTrust = 0.70 // fraction of domains with a reliable sitemap lastmod
)

// Policy is one of the four scheduling stacks compared in §8.
type Policy int

const (
	// PolicyUniform is (a): today's unordered LIMIT draw — no learning, no
	// prioritization, global 120d recrawl floor, full fetch cost always.
	PolicyUniform Policy = iota
	// PolicyPriority is (b): §5 priority draw P = importance ×
	// expected_gain × overdue, with the change_rate EWMA learned from
	// fetch verdicts and T3-style adaptive intervals.
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
	importance float64 // Zipf: 1/rank
	lambda     float64 // true changes/day (hidden from the scheduler)
	changes    []int   // sorted days on which the page truly changed
	sitemapOK  bool    // sitemap lastmod is a reliable staleness hint
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
// processes, 70% reliable sitemap lastmods. Deterministic for a seed.
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
			importance: 1.0 / float64(i+1), // rank i+1, Zipf
			lambda:     lam,
			changes:    ch,
			sitemapOK:  rng.Float64() < sitemapTrust,
		}
		w.impSum += w.domains[i].importance
		w.initCrawl[i] = -rng.Intn(int(intervalMax)) // staggered start: due spread over 120d
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
}

// Metrics collected over a run.
type Metrics struct {
	LagDays    float64     // mean importance-weighted staleness, in days (§8)
	Fetches    int         // total fetches issued
	SpentTotal float64     // total budget units spent
	Spent304   float64     // budget units spent on unchanged (304) fetches
	MaxOverdue [10]float64 // max age/interval per importance decile (starvation check)
}

// Pct304 returns the share of budget spent on unchanged fetches.
func (m Metrics) Pct304() float64 {
	if m.SpentTotal == 0 {
		return 0
	}
	return 100 * m.Spent304 / m.SpentTotal
}

type schedState struct {
	lamHat    float64 // current change_rate estimate (changes/day)
	chgFrac   float64 // EWMA of P(fetch observed a change)
	fetchGap  float64 // EWMA of days between fetches
	lastCrawl int
}

func newSchedState() schedState {
	// Consistent with the prior: a domain fetched every 120d whose fetches
	// observe a change with P = 1-exp(-1) has λ̂ = 1/120.
	return schedState{
		lamHat:    priorLambda,
		chgFrac:   1 - math.Exp(-1),
		fetchGap:  intervalMax,
		lastCrawl: 0,
	}
}

// learn updates the change_rate estimate from one fetch verdict.
// The verdict is interval-censored (we only know "changed at least once in
// `age` days"), so the estimator is the Poisson MLE λ̂ = -ln(1-f)/ā from the
// EWMA changed-fraction f and EWMA fetch interval ā — not the naive 1/age,
// which is a fixed point of due-triggered fetching and never converges.
func (s *schedState) learn(changed bool, age int) {
	if age < 1 {
		age = 1
	}
	f := 0.0
	if changed {
		f = 1.0
	}
	s.chgFrac = (1-ewmaAlpha)*s.chgFrac + ewmaAlpha*f
	s.fetchGap = (1-ewmaAlpha)*s.fetchGap + ewmaAlpha*float64(age)
	s.lamHat = clamp(-math.Log(1-s.chgFrac)/s.fetchGap, lambdaMin, lambdaMax)
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
			age := float64(day - s.lastCrawl)
			var interval float64
			if pol == PolicyUniform {
				interval = intervalMax // today's global RECRAWL_DAYS floor
			} else {
				interval = clamp(1/s.lamHat, intervalMin, intervalMax)
			}
			if age < interval {
				continue // not due
			}
			p := 0.0
			if pol != PolicyUniform {
				overdue := age / interval
				gain := 1 - math.Exp(-s.lamHat*age) // expected_gain (§4)
				p = d.importance * gain * overdue
				if (pol == PolicySitemap || pol == PolicyFull) && d.hinted(day, s.lastCrawl) {
					p *= sitemapBoost
				}
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
			if remaining < fetchCost && !(pol == PolicyFull && remaining >= notModCost) {
				break
			}
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
			// 3. Learn: interval-censored Poisson MLE on the fetch verdict (§4).
			if pol != PolicyUniform {
				st[i].learn(changed, day-s.lastCrawl)
			}
			s.lastCrawl = day
		}

		// 4. Accumulate importance-weighted staleness (§8 lag metric).
		for i, d := range w.domains {
			lc := d.lastChangeDay(day)
			if lc > st[i].lastCrawl {
				m.LagDays += d.importance * float64(day-lc)
			}
		}
	}

	m.LagDays /= float64(w.days) * w.impSum

	// 5. Starvation check: max overdue per importance decile at end of run.
	for i := range w.domains {
		s := &st[i]
		age := float64(w.days - s.lastCrawl)
		var interval float64
		if pol == PolicyUniform {
			interval = intervalMax
		} else {
			interval = clamp(1/s.lamHat, intervalMin, intervalMax)
		}
		od := age / interval
		dec := w.decile[i]
		if od > m.MaxOverdue[dec] {
			m.MaxOverdue[dec] = od
		}
	}
	_ = n
	return m
}
