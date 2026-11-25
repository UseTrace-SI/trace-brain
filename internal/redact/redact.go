package redact

import (
	"math"
	"regexp"
	"strings"
)

type Redactor struct{ PII bool }

var patterns = []struct {
	kind string
	rx   *regexp.Regexp
}{
	{"private-key", regexp.MustCompile(`(?s)(-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----.*?-----END (?:[A-Z0-9 ]+ )?PRIVATE KEY-----|-----BEGIN PGP PRIVATE KEY BLOCK-----.*?-----END PGP PRIVATE KEY BLOCK-----)`)},
	{"aws-key", regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},
	{"aws-secret", regexp.MustCompile(`(?i)\b(?:aws[ _-]*)?secret[ _-]*(?:access[ _-]*)?key[^\S\n]*[:=][^\S\n]*["']?[A-Za-z0-9/+=]{40}`)},
	{"google-api-key", regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{35}\b`)},
	{"huggingface-token", regexp.MustCompile(`\bhf_[A-Za-z0-9]{20,}\b`)},
	{"npm-token", regexp.MustCompile(`\bnpm_[A-Za-z0-9]{20,}\b`)},
	{"github-token", regexp.MustCompile(`\b(?:ghp_|gho_|ghu_|ghs_|ghr_|github_pat_)[A-Za-z0-9_]{20,}\b`)},
	{"slack-token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},
	{"stripe-key", regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}\b`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)},
	{"api-key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`)},
	{"url-password", regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s:/@]+:[^\s@/]+@`)},
	// HTTP credentials rarely carry a recognizable prefix or a key-context
	// word, so an opaque bearer token in a pasted curl command used to pass.
	{"authorization", regexp.MustCompile(`(?i)\bauthorization["']?\s*[:=]\s*["']?(?:bearer|basic|token|bot)\s+[A-Za-z0-9._~+/=-]{8,}`)},
	{"bearer-token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{20,}=*`)},
}

// Necessary literal fragments cheaply rule out patterns before invoking the
// regexp engine on large transcripts. A fragment alone never causes redaction.
var patternHints = map[string][]string{
	"private-key":       {"PRIVATE KEY-----", "PGP PRIVATE KEY BLOCK-----"},
	"aws-key":           {"AKIA", "ASIA"},
	"aws-secret":        {"SECRET", "secret"},
	"google-api-key":    {"AIza"},
	"huggingface-token": {"hf_"},
	"npm-token":         {"npm_"},
	"github-token":      {"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"},
	"slack-token":       {"xox"},
	"stripe-key":        {"sk_", "rk_"},
	"jwt":               {"eyJ"},
	"api-key":           {"sk-"},
	"url-password":      {"://"},
	"authorization":     {"uthorization", "UTHORIZATION"},
	"bearer-token":      {"earer", "EARER"},
}

// The optional quote after the name also covers quoted JSON/YAML keys such as
// "db_password": "value", which previously escaped redaction entirely.
var envValue = regexp.MustCompile(`(?im)\b[A-Z0-9_]*(?:KEY|SECRET|TOKEN|PASSWORD|PASSWD)[A-Z0-9_]*["']?\s*[:=]\s*(["']?[^\s,"']+["']?)`)
var redactionMarker = regexp.MustCompile(`⟨redacted:[a-z-]+:[0-9a-f]{8}⟩`)
var email = regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)
var phone = regexp.MustCompile(`\b\+?[0-9][0-9 ()-]{8,}[0-9]\b`)
var candidate = regexp.MustCompile(`[A-Za-z0-9_+/=-]{20,}`)
var keyContext = regexp.MustCompile(`(?i)(key|secret|token|password|passwd|api)`)

func (r Redactor) Text(s string) string {
	if s == "" {
		return s
	}
	for _, p := range patterns {
		possible := false
		for _, hint := range patternHints[p.kind] {
			if strings.Contains(s, hint) {
				possible = true
				break
			}
		}
		if !possible {
			continue
		}
		s = p.rx.ReplaceAllStringFunc(s, func(v string) string { return marker(p.kind, v) })
	}
	context := hasKeyContext(s)
	if context {
		s = redactEnvValues(s)
	}
	// Every decision below reads the unmodified original, so replacements are
	// assembled in one forward pass; rebuilding the string per replacement
	// was quadratic in the number of redactions.
	original := s
	var out strings.Builder
	last := 0
	for _, index := range candidate.FindAllStringIndex(original, -1) {
		start, end := index[0], index[1]
		from := start - 30
		if from < 0 {
			from = 0
		}
		value := original[start:end]
		highEntropy := entropy(value) > 4.0
		// A bare 40-character AWS secret key (mixed upper/lower/digits) is
		// recognized even without a nearby key label. Pure hex digests and
		// UUIDs lack the mixed classes and remain untouched.
		secretKey := looksLikeAWSKey(value)
		// A redaction marker in the preceding window means the surrounding
		// text is already in a secret-bearing region. This used to *skip* the
		// candidate, so a secret placed right after a marker-shaped literal
		// evaded entropy redaction. Markers are never candidates themselves
		// (no 20+ char alphanumeric run), so treating them as key context is
		// both idempotent and closes the bypass.
		markerContext := strings.Contains(original[from:start], "⟨redacted:")
		if !highEntropy || (!keyContext.MatchString(original[from:start]) && !markerContext && !secretKey) {
			continue
		}
		out.WriteString(original[last:start])
		out.WriteString(marker("entropy", value))
		last = end
	}
	if last > 0 {
		out.WriteString(original[last:])
		s = out.String()
	}
	if r.PII {
		s = email.ReplaceAllStringFunc(s, func(v string) string { return marker("email", v) })
		s = phone.ReplaceAllStringFunc(s, func(v string) string { return marker("phone", v) })
	}
	return s
}

func hasKeyContext(s string) bool {
	lower := strings.ToLower(s)
	for _, word := range []string{"key", "secret", "token", "password", "passwd", "api"} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	// Unicode case-insensitive regular expressions also fold long s to S.
	return strings.ContainsRune(s, '\u017f') && keyContext.MatchString(s)
}

func redactEnvValues(s string) string {
	// Every environment assignment requires a separator. Avoid the expensive
	// case-insensitive name matcher on large source/transcript strings without
	// one; this preserves exactly the same redaction behavior.
	if !strings.ContainsAny(s, "=:") {
		return s
	}
	matches := envValue.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		return s
	}
	markers := redactionMarker.FindAllStringIndex(s, -1)
	var out strings.Builder
	last, nextMarker := 0, 0
	for _, match := range matches {
		for nextMarker < len(markers) && markers[nextMarker][1] <= match[0] {
			nextMarker++
		}
		if nextMarker < len(markers) && markers[nextMarker][0] <= match[0] && match[0] < markers[nextMarker][1] {
			continue
		}
		value := s[match[0]:match[1]]
		i := strings.IndexAny(value, "=:")
		if i < 0 {
			continue
		}
		unquoted := strings.Trim(strings.TrimSpace(value[i+1:]), `"'`)
		// An already-redacted value may be followed by closing punctuation,
		// e.g. a quoted JSON value: keep a second pass a fixed point.
		if location := redactionMarker.FindStringIndex(unquoted); location != nil && location[0] == 0 && strings.Trim(unquoted[location[1]:], "}])>;") == "" {
			continue
		}
		out.WriteString(s[last:match[0]])
		out.WriteString(value[:i+1])
		out.WriteString(marker("env", strings.TrimSpace(value[i+1:])))
		last = match[1]
	}
	out.WriteString(s[last:])
	return out.String()
}

// looksLikeAWSKey reports whether s has the shape of a 40-character AWS
// secret access key: mixed character classes and a base64-like alphabet.
func looksLikeAWSKey(s string) bool {
	if len(s) != 40 {
		return false
	}
	var upper, lower, digit bool
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= '0' && c <= '9':
			digit = true
		case c == '/' || c == '+' || c == '=' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return upper && lower && digit
}

func entropy(s string) float64 {
	if s == "" {
		return 0
	}
	counts := map[byte]int{}
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	var result float64
	for _, n := range counts {
		p := float64(n) / float64(len(s))
		result -= p * math.Log2(p)
	}
	return result
}
