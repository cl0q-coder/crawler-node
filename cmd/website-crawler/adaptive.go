package main

// T3: adaptive recrawl interval — pure math, unit-tested.
//
// Per-domain recrawl interval adapts to observed change behavior:
//   - 304 Not Modified (unchanged) → interval *= 1.25, capped at 30 days.
//   - 200 with content (changed)  → interval *= 0.75, floored at 1 day.
// A zero/negative input means "no interval set yet" → the global default.

const (
	// t3IntervalFloorDays is the minimum recrawl interval (daily for news-like sites).
	t3IntervalFloorDays = 1.0
	// t3IntervalCapDays is the maximum recrawl interval (monthly for stable pages).
	t3IntervalCapDays = 30.0
	// t3BackoffFactor widens the interval when a page is unchanged.
	t3BackoffFactor = 1.25
	// t3TightenFactor narrows the interval when a page changed.
	t3TightenFactor = 0.75
)

// t3NextInterval returns the new recrawl interval in days given the current
// interval and whether the page changed. A current <= 0 means unset.
func t3NextInterval(current float64, changed bool, def float64) float64 {
	if current <= 0 {
		current = def
	}
	if changed {
		if v := current * t3TightenFactor; v < t3IntervalFloorDays {
			return t3IntervalFloorDays
		} else {
			return v
		}
	}
	if v := current * t3BackoffFactor; v > t3IntervalCapDays {
		return t3IntervalCapDays
	} else {
		return v
	}
}
