package redact

import (
	"strings"
	"testing"
	"time"
)

// syntheticAWSSecret builds a 40-character mixed-class value with high
// entropy without embedding a credential-shaped literal in the source tree.
func syntheticAWSSecret() string {
	const alphabet = "Ab3Cd5Ef7Gh9Jk1Lm2Np4Qr6"
	value := make([]byte, 40)
	for i := range value {
		value[i] = alphabet[i%len(alphabet)]
	}
	return string(value)
}

func TestKnownSecrets(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"aws", "AK" + "IA" + strings.Repeat("Z", 16)},
		{"github", "ghp_abcdefghijklmnopqrstuvwxyz012345"},
		{"openai", "sk-abcdefghijklmnopqrstuvwxyz012345"},
		{"env", "API_TOKEN=abcdefghijklmnopqrstuvwxyz012345"},
		{"unicode folded context", "ſecret value 7dF2aQ9mR4xT8vL1pN6cY3zB5kH0sW2u"},
		{"private key", "-----BEGIN PRIVATE KEY-----\nmaterial\n-----END PRIVATE KEY-----"},
		{"dsa private key", "-----BEGIN DSA PRIVATE KEY-----\nmaterial\n-----END DSA PRIVATE KEY-----"},
		{"encrypted private key", "-----BEGIN ENCRYPTED PRIVATE KEY-----\nmaterial\n-----END ENCRYPTED PRIVATE KEY-----"},
		{"google api key", "AIzaABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"},
		{"huggingface token", "hf_abcdefghijklmnopqrstuvwxyz0123456789"},
		{"npm token", "npm_abcdefghijklmnopqrstuvwxyz0123456789"},
		{"aws secret key", syntheticAWSSecret()},
		{"slack", "xoxb-1234567890-abcdefghij"},
		{"stripe", "sk_live_abcdefghijklmnop123456"},
		{"jwt", "eyJabcdefghij.eyJabcdefghij.abcdefghijk"},
		{"URL password", "postgres://user:password@example.invalid/db"},
		{"entropy", "api value 7dF2aQ9mR4xT8vL1pN6cY3zB5kH0sW2u"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := (Redactor{}).Text(tc.input)
			if strings.Contains(got, tc.input) || !strings.Contains(got, "⟨redacted:") {
				t.Fatalf("unredacted: %q", got)
			}
		})
	}
}

func TestLargeUnrelatedTextIsUnchanged(t *testing.T) {
	value := strings.Repeat("x", 1<<20)
	if got := (Redactor{}).Text(value); got != value {
		t.Fatal("unrelated large text changed")
	}
}

func TestNegativesAndPII(t *testing.T) {
	for _, value := range []string{"0123456789abcdef0123456789abcdef01234567", "550e8400-e29b-41d4-a716-446655440000", "ordinary request"} {
		if got := (Redactor{}).Text(value); got != value {
			t.Errorf("false positive: %q -> %q", value, got)
		}
	}
	// A 40-character lowercase hex digest is not an AWS secret key: the
	// high-entropy fallback must require mixed character classes.
	digest := strings.Repeat("0123456789abcdef", 2) + "01234567"
	if got := (Redactor{}).Text(digest); got != digest {
		t.Errorf("hex digest was redacted: %q -> %q", digest, got)
	}
	if got := (Redactor{PII: true}).Text("contact test@example.com"); strings.Contains(got, "test@example.com") {
		t.Fatalf("email leaked: %q", got)
	}
}

// A marker-shaped string immediately before a high-entropy token must not turn
// off entropy redaction for that token. The preceding-context check used to
// skip the candidate whenever a marker was nearby, letting a secret placed
// right after a marker-shaped literal evade redaction entirely.
func TestRedactionMarkerDoesNotHideFollowingSecret(t *testing.T) {
	// 20 distinct letters have entropy log2(20) > 4.0 and no key-context word,
	// so only the entropy fallback (or marker context) can redact this value.
	secret := "abcdefghijklmnopqrst"
	for _, input := range []string{
		marker("env", "example") + " " + secret,
		"KEY=" + marker("env", "example") + " " + secret,
	} {
		got := (Redactor{}).Text(input)
		if strings.Contains(got, secret) || !strings.Contains(got, "⟨redacted:entropy:") {
			t.Fatalf("secret after a marker was not redacted: %q", got)
		}
	}
}

func TestRedactionMarkersAreStable(t *testing.T) {
	for _, tc := range []struct {
		name, input, secret string
	}{
		{"known token", "turn ghp_abcdefghijklmnopqrstuvwxyz012345", "ghp_abcdefghijklmnopqrstuvwxyz012345"},
		{"environment value", "API_TOKEN=ordinarysecret", "ordinarysecret"},
		{"token as environment value", "API_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz012345", "ghp_abcdefghijklmnopqrstuvwxyz012345"},
		{"value after existing marker", marker("github-token", "example") + " API_TOKEN=ordinarysecret", "ordinarysecret"},
		{"marker inside environment value", "API_TOKEN=" + marker("github-token", "example") + "ordinarysecret", "ordinarysecret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redactor := Redactor{}
			once := redactor.Text(tc.input)
			if strings.Contains(once, tc.secret) || !strings.Contains(once, "⟨redacted:") {
				t.Fatalf("secret was not redacted: %q", once)
			}
			if twice := redactor.Text(once); twice != once {
				t.Fatalf("redaction changed on a second pass: %q -> %q", once, twice)
			}
		})
	}
}

// These credential shapes used to pass redaction unchanged (PGP bodies beyond
// the first line, quoted JSON keys, opaque HTTP bearer/basic credentials),
// so they reached manual, MCP, document and import memories verbatim.
func TestRegressionRedactsPGPKeysQuotedKeysAndHTTPCredentials(t *testing.T) {
	opaque := strings.Repeat("9f8e7d6c5b4a3928", 2) + "Zz"
	pgpBody := "lQOYBF0" + strings.Repeat("abcdefghijklmnopqrstuvwxyzABCDEF", 2) + "\n" + strings.Repeat("qrstuvwxyzABCDEFGHIJ0123456789", 2)
	for _, tc := range []struct{ name, input, secret string }{
		{"pgp private key block", "-----BEGIN PGP PRIVATE KEY BLOCK-----\n\n" + pgpBody + "\n-----END PGP PRIVATE KEY BLOCK-----", "qrstuvwxyzABCDEFGHIJ0123456789"},
		{"quoted JSON key", `{"db_password": "hunter2"}`, "hunter2"},
		{"quoted JSON key without spaces", `{"api_key":"q8Zr2LmN"}`, "q8Zr2LmN"},
		{"single-quoted YAML key", `'client_secret': 'v4lue'`, "v4lue"},
		{"authorization bearer header", `curl -H "Authorization: Bearer ` + opaque + `" https://example.invalid`, opaque},
		{"authorization basic header", "Authorization: Basic dXNlcjpwYXNzd29yZA==", "dXNlcjpwYXNzd29yZA"},
		{"bare bearer token", "send bearer " + opaque + " upstream", opaque},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redactor := Redactor{}
			once := redactor.Text(tc.input)
			if strings.Contains(once, tc.secret) || !strings.Contains(once, "⟨redacted:") {
				t.Fatalf("unredacted: %q", once)
			}
			if twice := redactor.Text(once); twice != once {
				t.Fatalf("redaction changed on a second pass: %q -> %q", once, twice)
			}
		})
	}
	for _, value := range []string{"the bearer of bad news", "Authorization header is required", `{"status": "ok", "count": 3}`} {
		if got := (Redactor{}).Text(value); got != value {
			t.Errorf("false positive: %q -> %q", value, got)
		}
	}
}

// The entropy pass rebuilt the whole string for each replacement, so text with
// many secrets took quadratic time (2 MiB of dense secrets: about 26 s).
func TestDenseRedactionIsLinearAndComplete(t *testing.T) {
	line := "api value 7dF2aQ9mR4xT8vL1pN6cY3zB5kH0sW2u\n"
	text := strings.Repeat(line, (2<<20)/len(line))
	start := time.Now()
	out := (Redactor{}).Text(text)
	// The absolute budget is enforced only by the ordinary (non-race) build.
	if elapsed := time.Since(start); !raceEnabled && elapsed > 10*time.Second {
		t.Fatalf("dense redaction took %s", elapsed)
	}
	if strings.Contains(out, "7dF2aQ9mR4xT8vL1pN6cY3zB5kH0sW2u") || strings.Count(out, "⟨redacted:entropy:") != strings.Count(text, "\n") {
		t.Fatal("dense secrets were not all redacted")
	}
	if (Redactor{}).Text(out) != out {
		t.Fatal("dense redaction is not a fixed point")
	}
}
