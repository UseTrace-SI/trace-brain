package semantic

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestGenerateIsDeterministicSchemaBoundAndValidatesTheReply(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	var seen map[string]any
	provider := testOllama(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		seen = map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Error(err)
		}
		w.Write([]byte(`{"model":"m:1","response":"{\"claims\":[]}","done":true,"done_reason":"stop"}`))
	}))
	out, err := provider.Generate(context.Background(), "m:1", "extract", schema)
	if err != nil || out != `{"claims":[]}` {
		t.Fatalf("generate: %q %v", out, err)
	}
	options, _ := seen["options"].(map[string]any)
	if seen["stream"] != false || seen["model"] != "m:1" || options["temperature"] != float64(0) || options["num_ctx"] == nil || seen["format"] == nil {
		t.Fatalf("request was not deterministic and schema-bound: %+v", seen)
	}
	for _, body := range []string{
		`{"model":"other","response":"{}","done":true}`,
		`{"model":"m:1","response":"{}","done":false}`,
		`{"model":"m:1","response":"{}","done":true,"done_reason":"length"}`,
		`{"model":"m:1","response":"` + strings.Repeat("x", MaxGenerateResponseBytes+1) + `","done":true}`,
		`not json`,
	} {
		provider := testOllama(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		if _, err := provider.Generate(context.Background(), "m:1", "extract", schema); err == nil {
			t.Errorf("accepted %.60s", body)
		}
	}
	for _, prompt := range []string{"", strings.Repeat("p", MaxGeneratePromptBytes+1), "bad \xff"} {
		if _, err := provider.Generate(context.Background(), "m:1", prompt, schema); err == nil {
			t.Errorf("accepted prompt of %d bytes", len(prompt))
		}
	}
	if _, err := provider.Generate(context.Background(), "m:1", "extract", json.RawMessage(`{`)); err == nil {
		t.Error("accepted an invalid schema")
	}
}
