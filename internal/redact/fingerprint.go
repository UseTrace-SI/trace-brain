package redact

import (
	"crypto/sha256"
	"encoding/hex"
)

// AlgorithmVersion identifies the redaction algorithm used when deriving
// content fingerprints. Bump it whenever redaction output can change so that
// cached learned vectors are detected as stale; the memory store must then
// re-fingerprint stored memories in a new schema migration.
const AlgorithmVersion = "redact-v2"

// Fingerprint binds text to the exact redacted bytes a model would receive
// under the current redaction algorithm.
func Fingerprint(content string) string {
	return FingerprintVersion(content, AlgorithmVersion)
}

// FingerprintVersion computes the fingerprint for an explicit algorithm
// version. It is exposed so tests can model vectors from older releases.
func FingerprintVersion(content, version string) string {
	sum := sha256.Sum256([]byte(version + "\x00" + (Redactor{}).Text(content)))
	return hex.EncodeToString(sum[:])
}
