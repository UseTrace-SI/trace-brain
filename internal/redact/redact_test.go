package redact

import (
	"strings"
	"testing"
)

func TestKnownSecrets(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"aws", "AKIAABCDEFGHIJKLMNOP"},
		{"github", "ghp_abcdefghijklmnopqrstuvwxyz012345"},
		{"openai", "sk-abcdefghijklmnopqrstuvwxyz012345"},
		{"env", "API_TOKEN=abcdefghijklmnopqrstuvwxyz012345"},
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

func TestNegativesAndPII(t *testing.T) {
	for _, value := range []string{"0123456789abcdef0123456789abcdef01234567", "550e8400-e29b-41d4-a716-446655440000", "ordinary request"} {
		if got := (Redactor{}).Text(value); got != value {
			t.Errorf("false positive: %q -> %q", value, got)
		}
	}
	if got := (Redactor{PII: true}).Text("contact test@example.com"); strings.Contains(got, "test@example.com") {
		t.Fatalf("email leaked: %q", got)
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
