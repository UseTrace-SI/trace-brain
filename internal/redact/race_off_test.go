//go:build !race

package redact

// raceEnabled reports a -race build; see race_on_test.go.
const raceEnabled = false
