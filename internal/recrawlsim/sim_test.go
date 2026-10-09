package recrawlsim

import (
	"fmt"
	"testing"
)

// TestHE4PolicyComparison runs the design-note §8 experiment: the four
// scheduling policies against one fixed synthetic web at a constrained
// budget, and checks the §8 success bar — the full stack (d) cuts
// importance-weighted lag by >=50% vs today's uniform draw (a), with no
// importance decile starved (max overdue <= 2x its interval).
func TestHE4PolicyComparison(t *testing.T) {
	const domains = 20000
	const days = 360
	const warmup = 120
	// Budget 800/day ≈ 72% of the full stack's steady-state need
	// (~1116/day): genuinely constrained — the crawler cannot fetch
	// everything due — but adequate for a good scheduler to keep up
	// without starving any decile. (Uniform's own need is ~2297/day:
	// legacy T3 misreads validator-less 200s as "changed" and tightens,
	// more than doubling its crawl load — part of what HE-4 fixes.)
	const budget = 800.0

	w := generate(domains, days, 20261007)
	pols := []Policy{PolicyUniform, PolicyPriority, PolicySitemap, PolicyFull}
	res := make(map[Policy]Metrics)
	for i, p := range pols {
		res[p] = Run(w, p, Config{Budget: budget, Seed: int64(1000 + i), Warmup: warmup})
	}

	fmt.Printf("\nHE-4 §8 simulation: %d domains, %d days (warmup %d), budget %.0f/day\n",
		domains, days, warmup, budget)
	fmt.Printf("%-22s %12s %12s %10s %12s\n", "policy", "lag(days)", "vs uniform", "%budget304", "maxOverdue")
	base := res[PolicyUniform].LagDays
	for _, p := range pols {
		m := res[p]
		maxOD := 0.0
		for _, od := range m.MaxOverdue {
			if od > maxOD {
				maxOD = od
			}
		}
		fmt.Printf("%-22s %12.2f %11.1f%% %9.1f%% %12.2f\n",
			p, m.LagDays, 100*m.LagDays/base, m.Pct304(), maxOD)
	}

	full := res[PolicyFull]
	if full.LagDays > 0.5*base {
		t.Errorf("success bar missed: full stack lag %.2f > 50%% of uniform %.2f",
			full.LagDays, base)
	}
	for dec, od := range full.MaxOverdue {
		if od > 2.0 {
			t.Errorf("starvation: importance decile %d max overdue %.2f > 2x interval", dec, od)
		}
	}
	// NOTE on (b)/(c) vs (a): the priority draw alone does NOT beat uniform
	// with the specified estimator. The §4 binary-observation EWMA converges
	// to P(change per fetch), not changes/day, so expected_gain = 1-exp(-λ̂·age)
	// is miscalibrated (e.g. a yearly changer's λ̂→0.08 reads as "90% likely
	// changed" at age 30d when truth is 8%). An interval-censored Poisson MLE
	// (λ̂ = -ln(1-f)/ā) fixes it — priority then beats uniform 1.50 vs 1.77 —
	// but that changes the specified update rule, so it is filed as a
	// follow-up on #146 rather than snuck in here. The design's §8 bar is on
	// the full stack (d), which passes via correct SimHash verdicts (halving
	// crawl load vs legacy T3's validator-blind tightening) and the 304
	// cost model.
}

// TestHE4BudgetCurve prints lag-vs-budget curves per policy (design note §8):
// the knee where extra budget stops buying freshness. Informational only —
// asserts monotonic improvement with budget as a sanity check.
func TestHE4BudgetCurve(t *testing.T) {
	const domains = 20000
	const days = 360
	const warmup = 120
	w := generate(domains, days, 20261007)
	pols := []Policy{PolicyUniform, PolicyPriority, PolicySitemap, PolicyFull}
	budgets := []float64{400, 800, 1600}

	fmt.Printf("\nHE-4 lag-vs-budget curve: %d domains, %d days (warmup %d)\n", domains, days, warmup)
	fmt.Printf("%-22s", "policy")
	for _, b := range budgets {
		fmt.Printf(" %10.0f/d", b)
	}
	fmt.Println()
	prev := make(map[Policy]float64)
	for _, p := range pols {
		fmt.Printf("%-22s", p)
		for _, b := range budgets {
			m := Run(w, p, Config{Budget: b, Seed: 4242, Warmup: warmup})
			fmt.Printf(" %10.2f", m.LagDays)
			if prevLag, ok := prev[p]; ok && m.LagDays > prevLag*1.001 {
				t.Errorf("policy %s: lag rose with budget %.0f -> %.0f (%.2f -> %.2f)",
					p, b/2, b, prevLag, m.LagDays)
			}
			prev[p] = m.LagDays
		}
		fmt.Println()
	}
}
