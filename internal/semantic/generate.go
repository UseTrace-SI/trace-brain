package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"unicode/utf8"
)

// Generator is an explicitly configured local completion model. It shares
// the loopback-only, redirect-free, size- and time-bounded Ollama client.
type Generator interface {
	Origin() string
	Digest(context.Context, string) (string, error)
	Generate(context.Context, string, string, json.RawMessage) (string, error)
	Close()
}

// GeneratorFactory constructs a generator for an owner-selected endpoint.
type GeneratorFactory func(string) (Generator, error)

const (
	// MaxGeneratePromptBytes bounds one completion prompt.
	MaxGeneratePromptBytes = 24 << 10
	// MaxGenerateResponseBytes bounds the text a completion may return.
	MaxGenerateResponseBytes = 64 << 10
	// generateContextTokens fixes the model context so a prompt within
	// MaxGeneratePromptBytes is not silently truncated by a small default.
	generateContextTokens = 8192
	generatePredictTokens = 2048
)

// ErrCompletionTruncated reports a completion cut off at its token limit. It
// concerns one input, unlike transport or identity failures.
var ErrCompletionTruncated = errors.New("model completion reached its token limit")

// Generate runs one deterministic, non-streaming completion whose output is
// constrained by the JSON schema format. The model identity and completion
// state are checked, and the response is bounded.
func (o *Ollama) Generate(ctx context.Context, model, prompt string, format json.RawMessage) (string, error) {
	if err := ValidateModel(model); err != nil {
		return "", err
	}
	if prompt == "" || len(prompt) > MaxGeneratePromptBytes || !utf8.ValidString(prompt) {
		return "", fmt.Errorf("completion prompt must contain 1–%d valid UTF-8 bytes", MaxGeneratePromptBytes)
	}
	if len(format) == 0 || !json.Valid(format) {
		return "", fmt.Errorf("completion format must be a JSON schema")
	}
	body, err := json.Marshal(map[string]any{"model": model, "prompt": prompt, "stream": false, "format": format,
		"options": map[string]any{"temperature": 0, "seed": 0, "num_ctx": generateContextTokens, "num_predict": generatePredictTokens}})
	if err != nil {
		return "", err
	}
	var out struct {
		Model      string `json:"model"`
		Response   string `json:"response"`
		Done       bool   `json:"done"`
		DoneReason string `json:"done_reason"`
	}
	if err := o.request(ctx, http.MethodPost, "/api/generate", body, &out); err != nil {
		return "", err
	}
	if out.Model != model || !out.Done {
		return "", fmt.Errorf("model identity or completion state mismatch")
	}
	if out.DoneReason == "length" {
		return "", ErrCompletionTruncated
	}
	if out.DoneReason != "" && out.DoneReason != "stop" {
		return "", fmt.Errorf("model completion ended unexpectedly")
	}
	if len(out.Response) > MaxGenerateResponseBytes || !utf8.ValidString(out.Response) {
		return "", fmt.Errorf("model completion exceeds %d bytes or is invalid UTF-8", MaxGenerateResponseBytes)
	}
	return out.Response, nil
}
