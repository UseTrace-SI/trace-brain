//go:build !race

package mcp

// raceSlowdown scales liveness waits; see race_on_test.go.
const raceSlowdown = 1
