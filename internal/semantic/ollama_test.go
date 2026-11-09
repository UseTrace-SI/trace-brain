package semantic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type handlerTransport struct{ handler http.Handler }

func (h handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, request)
	return recorder.Result(), nil
}
func testOllama(t *testing.T, handler http.Handler) *Ollama {
	t.Helper()
	provider, err := NewOllama("http://127.0.0.1:11434")
	if err != nil {
		t.Fatal(err)
	}
	provider.client.Transport = handlerTransport{handler}
	t.Cleanup(provider.Close)
	return provider
}

func TestOllamaRefusesRemoteOriginsAndCredentials(t *testing.T) {
	for _, endpoint := range []string{"https://example.com", "http://127.0.0.1.evil.test", "http://user:secret@localhost:11434", "http://localhost:11434/path", "http://localhost:11434?token=secret", "file:///tmp/model", "http://0.0.0.0:11434"} {
		if _, err := NewOllama(endpoint); err == nil {
			t.Fatalf("accepted %s", endpoint)
		}
	}
}

func TestOllamaValidatesIdentityDigestAndBatchAndForbidsTruncation(t *testing.T) {
	provider := testOllama(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": "test:1", "digest": strings.Repeat("a", 64)}}})
		case "/api/embed":
			var request struct {
				Model    string   `json:"model"`
				Input    []string `json:"input"`
				Truncate *bool    `json:"truncate"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.Model != "test:1" || request.Truncate == nil || *request.Truncate || len(request.Input) != 2 {
				t.Errorf("unexpected request: %+v", request)
			}
			json.NewEncoder(w).Encode(map[string]any{"model": "test:1", "embeddings": [][]float32{{1, 0}, {0, 1}}})
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	if digest, err := provider.Digest(context.Background(), "test:1"); err != nil || digest != strings.Repeat("a", 64) {
		t.Fatalf("digest: %s %v", digest, err)
	}
	if _, err := provider.Digest(context.Background(), "test"); err == nil {
		t.Fatal("guessed a model alias")
	}
	if vectors, err := provider.Embed(context.Background(), "test:1", []string{"car", "automobile"}); err != nil || len(vectors) != 2 {
		t.Fatalf("vectors: %+v %v", vectors, err)
	}
}

func TestOllamaRejectsMalformedVectorsAndNeverFollowsRedirects(t *testing.T) {
	for _, body := range []string{`{"model":"other","embeddings":[[1,0]]}`, `{"model":"m","embeddings":[]}`, `{"model":"m","embeddings":[[0,0]]}`, `{"model":"m","embeddings":[[]]}`, `{"model":"m","embeddings":[[1,2],[1]]}`, `{"model":"m","embeddings":[[1e1000]]}`} {
		provider := testOllama(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		if _, err := provider.Embed(context.Background(), "m", []string{"private data"}); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	var redirected bool
	provider := testOllama(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Port() == "9999" {
			redirected = true
			return
		}
		http.Redirect(w, r, "http://127.0.0.1:9999/", 307)
	}))
	if _, err := provider.Embed(context.Background(), "m", []string{"private"}); err == nil || redirected {
		t.Fatal("followed model redirect")
	}
}
