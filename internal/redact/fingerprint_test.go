package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestFingerprintBindsRedactedTextAndAlgorithmVersion(t *testing.T) {
	secret := "ghp_abcdefghijklmnopqrstuvwxyz"
	raw := "deploy with " + secret
	redacted := (Redactor{}).Text(raw)
	if redacted == raw {
		t.Fatal("fixture secret was not redacted")
	}
	sum := sha256.Sum256([]byte(AlgorithmVersion + "\x00" + redacted))
	if got := Fingerprint(raw); got != hex.EncodeToString(sum[:]) || got != Fingerprint(redacted) {
		t.Fatalf("fingerprint does not bind the redacted text: %s", got)
	}
	if Fingerprint(raw) == FingerprintVersion(raw, "redact-v0") {
		t.Fatal("fingerprint ignores the algorithm version")
	}
	if Fingerprint("a") == Fingerprint("b") {
		t.Fatal("fingerprint ignores content")
	}
}
