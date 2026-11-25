//go:build !unix

package redact

// lockMarkerKey is a no-op where advisory file locks are unavailable;
// trace-brain supports macOS and Linux.
func lockMarkerKey(string) (func(), error) { return func() {}, nil }
