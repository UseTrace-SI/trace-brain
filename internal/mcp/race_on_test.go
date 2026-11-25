//go:build race

package mcp

// raceSlowdown scales liveness waits (how long a test waits for something
// that must happen) under the race detector. It stays small enough that a
// scaled wait remains well below the 30-second stalls these tests detect.
const raceSlowdown = 5
