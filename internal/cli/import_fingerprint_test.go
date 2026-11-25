package cli

import "testing"

// A process without the shared marker key labels secrets with its own key.
// Import change detection must not treat that as changed content.
func TestImportFingerprintIgnoresMarkerDigests(t *testing.T) {
	a, legacyA := importFingerprints("Turn t: token ⟨redacted:github-token:0123abcd⟩ done", "")
	b, legacyB := importFingerprints("Turn t: token ⟨redacted:github-token:89abcdef⟩ done", "")
	if a != b {
		t.Fatal("marker digests changed the import fingerprint")
	}
	if legacyA == legacyB {
		t.Fatal("legacy fingerprint should keep exact text, so old unkeyed markers are refreshed once")
	}
	if c, _ := importFingerprints("Turn t: token ⟨redacted:github-token:0123abcd⟩ changed", ""); c == a {
		t.Fatal("content change not detected")
	}
	if d, _ := importFingerprints("Turn t: token ⟨redacted:github-token:0123abcd⟩ done", "agent"); d == a {
		t.Fatal("agent command not bound")
	}
}
