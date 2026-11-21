//go:build race

package store

// raceSlowdown scales wall-clock budgets in timing tests. The race detector
// slows this package's pure-Go work by roughly an order of magnitude (CI exceeded a 2 s deadline that takes 0.5 s without it),
// while the regressions these budgets guard against are slower still.
const raceSlowdown = 15
