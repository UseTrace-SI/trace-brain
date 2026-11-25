//go:build race

package store

// raceEnabled reports a -race build. The race detector slows this package's
// pure-Go work by an unpredictable factor (CI once exceeded a 2 s budget that
// takes 0.5 s without it), so absolute performance budgets are enforced only
// by the ordinary build; the race build still checks correctness.
const raceEnabled = true
