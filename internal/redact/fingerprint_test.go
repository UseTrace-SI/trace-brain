package redact

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

func TestFingerprintBindsRedactedTextAndAlgorithmVersion(t *testing.T) {
	secret := "ghp_abcdefghijklmnopqrstuvwxyz"
	raw := "deploy with " + secret
	redacted := (Redactor{}).Text(raw)
	if redacted == raw {
		t.Fatal("fixture secret was not redacted")
	}
	sum := sha256.Sum256([]byte(AlgorithmVersion + "\x00" + "deploy with ⟨redacted:github-token⟩"))
	if got := Fingerprint(raw); got != hex.EncodeToString(sum[:]) || got != Fingerprint(redacted) {
		t.Fatalf("fingerprint does not bind the canonical redacted text: %s", got)
	}
	if Fingerprint(raw) == FingerprintVersion(raw, "redact-v2") {
		t.Fatal("fingerprint ignores the algorithm version")
	}
	if Fingerprint("a") == Fingerprint("b") {
		t.Fatal("fingerprint ignores content")
	}
}

// A missing shared key makes each process redact with its own key. The
// fingerprint must not change with it, or every stored fingerprint and cached
// vector would turn stale in every process.
func TestFingerprintIsIndependentOfTheMarkerKey(t *testing.T) {
	raw := "token ghp_abcdefghijklmnopqrstuvwxyz and API_KEY=hunter2hunter2"
	first, firstText := Fingerprint(raw), (Redactor{}).Text(raw)
	original := currentMarkerKey()
	markerKey.key = randomMarkerKey()
	defer func() { markerKey.key = original }()
	if (Redactor{}).Text(raw) == firstText {
		t.Fatal("markers did not depend on the key")
	}
	if Fingerprint(raw) != first {
		t.Fatal("fingerprint changed with the marker key")
	}
}

func TestCanonicalElidesOnlyMarkerDigests(t *testing.T) {
	in := "a ⟨redacted:github-token:0123abcd⟩ b ⟨redacted:env:ffffffff⟩ ⟨redacted:not-a-marker⟩ ⟨redacted:env:XYZ⟩"
	want := "a ⟨redacted:github-token⟩ b ⟨redacted:env⟩ ⟨redacted:not-a-marker⟩ ⟨redacted:env:XYZ⟩"
	if got := Canonical(in); got != want {
		t.Fatalf("Canonical = %q", got)
	}
	if Canonical("plain") != "plain" {
		t.Fatal("changed text without markers")
	}
}

// Markers must match the Trace CLI's for the same key file: HMAC-SHA256 of
// the value under the hex key in $TRACE_HOME/redaction.key, first 8 digits.
func TestMarkerMatchesTraceCLIDerivationForSharedKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TRACE_HOME", home)
	path, err := markerKeyPath()
	if err != nil || path != home+"/redaction.key" {
		t.Fatalf("key path %q %v", path, err)
	}
	t.Setenv("TRACE_HOME", "relative/dir")
	if _, err := markerKeyPath(); err == nil {
		t.Fatal("accepted a relative TRACE_HOME")
	}
	const hexKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	if err := os.WriteFile(path, []byte(hexKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := loadMarkerKey(path)
	if err != nil {
		t.Fatal(err)
	}
	original := currentMarkerKey()
	markerKey.key = key
	defer func() { markerKey.key = original }()
	raw, _ := hex.DecodeString(hexKey)
	mac := hmac.New(sha256.New, raw)
	mac.Write([]byte("hunter2"))
	want := "⟨redacted:env:" + hex.EncodeToString(mac.Sum(nil))[:8] + "⟩"
	if got := marker("env", "hunter2"); got != want {
		t.Fatalf("marker %q, want %q", got, want)
	}
}
