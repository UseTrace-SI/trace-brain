package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// AlgorithmVersion identifies the redaction algorithm used when deriving
// content fingerprints. Bump it whenever redaction output can change so that
// cached learned vectors are detected as stale; the memory store must then
// re-fingerprint stored memories in a new schema migration.
//
// redact-v3: markers are keyed (HMAC) and fingerprints elide marker digests.
const AlgorithmVersion = "redact-v3"

// Fingerprint binds text to the redacted bytes a model would receive under
// the current redaction algorithm, with each marker's digest elided (see
// Canonical). Markers depend on the per-install key, which may be ephemeral
// when the shared key file is unavailable; eliding the digests keeps
// fingerprints identical across processes and keys, so a missing key never
// makes stored fingerprints or cached vectors stale.
func Fingerprint(content string) string {
	return FingerprintVersion(content, AlgorithmVersion)
}

// FingerprintVersion computes the fingerprint for an explicit algorithm
// version. It is exposed so tests can model vectors from older releases.
func FingerprintVersion(content, version string) string {
	sum := sha256.Sum256([]byte(version + "\x00" + Canonical((Redactor{}).Text(content))))
	return hex.EncodeToString(sum[:])
}

// Canonical replaces every redaction marker's keyed digest with nothing
// ("⟨redacted:kind:1a2b3c4d⟩" becomes "⟨redacted:kind⟩"), so text compares
// equal regardless of which marker key produced it.
func Canonical(text string) string {
	if !strings.Contains(text, "⟨redacted:") {
		return text
	}
	return redactionMarker.ReplaceAllStringFunc(text, func(m string) string {
		kind := strings.TrimPrefix(m, "⟨redacted:")
		kind = kind[:strings.IndexByte(kind, ':')]
		return "⟨redacted:" + kind + "⟩"
	})
}
