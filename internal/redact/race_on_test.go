//go:build race

package redact

// raceSlowdown scales wall-clock budgets in timing tests. The race detector
// slows dense redaction about fivefold (10.8 s in CI against 2 s without it),
// while the quadratic behaviour these budgets guard against is far slower.
const raceSlowdown = 15
