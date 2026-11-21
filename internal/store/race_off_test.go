//go:build !race

package store

// raceSlowdown scales wall-clock budgets in timing tests; see race_on_test.go.
const raceSlowdown = 1
