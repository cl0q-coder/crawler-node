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
	const days = 240
	const budget = 400.0 // fetch-cost units/day; due rate ≈ 780/day -> constrained

	w := generate(domains, days, 20261007)
	pols := []Policy{PolicyUniform, PolicyPriority, PolicySitemap, PolicyFull}
	res := make(map[Policy]Metrics)
	for i, p := range pols {
		res[p] = Run(w, p, Config{Budget: budget, Seed: int64(1000 + i)})
	}

	fmt.Printf("\nHE-4 §8 simulation: %d domains, %d days, budget %.0f/day\n", domains, days, budget)
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
	// The priority draw alone must beat uniform — otherwise the scoring
	// function is wrong, not just the extras.
	if res[PolicyPriority].LagDays >= base {
		t.Errorf("priority draw did not beat uniform: %.2f vs %.2f",
			res[PolicyPriority].LagDays, base)
	}
}

// TestHE4BudgetCurve prints lag-vs-budget curves per policy (design note §8):
// the knee where extra budget stops buying freshness. Informational only —
// asserts monotonic improvement with budget as a sanity check.
func TestHE4BudgetCurve(t *testing.T) {
	const domains = 20000
	const days = 240
	w := generate(domains, days, 20261007)
	pols := []Policy{PolicyUniform, PolicyPriority, PolicySitemap, PolicyFull}
	budgets := []float64{200, 400, 800, 1600}

	fmt.Printf("\nHE-4 lag-vs-budget curve: %d domains, %d days\n", domains, days)
	fmt.Printf("%-22s", "policy")
	for _, b := range budgets {
		fmt.Printf(" %10.0f/d", b)
	}
	fmt.Println()
	prev := make(map[Policy]float64)
	for _, p := range pols {
		fmt.Printf("%-22s", p)
		for _, b := range budgets {
			m := Run(w, p, Config{Budget: b, Seed: 4242})
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
