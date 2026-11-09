package mcp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/embed"
	"github.com/UseTrace-SI/trace-brain/internal/semantic"
	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpModel struct {
	digest func(context.Context) (string, error)
	embed  func([]string)
	closed *atomic.Int64
}

func (*mcpModel) Origin() string { return "http://127.0.0.1:11434" }
func (m *mcpModel) Close() {
	if m.closed != nil {
		m.closed.Add(1)
	}
}
func (m *mcpModel) Digest(ctx context.Context, model string) (string, error) {
	if m.digest != nil {
		return m.digest(ctx)
	}
	return strings.Repeat("a", 64), ctx.Err()
}
func (m *mcpModel) Embed(ctx context.Context, model string, texts []string) ([][]float32, error) {
	if m.embed != nil {
		m.embed(texts)
	}
	return [][]float32{{1, 0}}, ctx.Err()
}
func semanticDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	text := "a car uses four wheels"
	if err := store.Remember(context.Background(), db, "m", "project", text, "manual", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO semantic_embeddings VALUES(?,?,?,?,?,?,?)", "m", "http://127.0.0.1:11434", "test:1", strings.Repeat("a", 64), fmt.Sprintf("%x", sha256.Sum256([]byte(text))), 2, embed.Encode([]float32{1, 0})); err != nil {
		t.Fatal(err)
	}
	return db
}
func semanticClient(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	a, b := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "semantic-test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestRegressionSemanticMCPIsOptInAndPinsProviderOverTransport(t *testing.T) {
	db := semanticDB(t)
	plain := semanticClient(t, NewServer(db))
	tools, err := plain.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "brain_semantic" {
			t.Fatal("ordinary server exposed network-backed tool")
		}
	}
	var calls, closed atomic.Int64
	secret := "ghp_abcdefghijklmnopqrstuvwxyz"
	factory := func(endpoint string) (semantic.Provider, error) {
		calls.Add(1)
		if endpoint != "http://127.0.0.1:11434" {
			t.Error("endpoint was not pinned")
		}
		return &mcpModel{closed: &closed, embed: func(texts []string) {
			if len(texts) != 1 || strings.Contains(texts[0], secret) {
				t.Error("query secret reached model")
			}
		}}, nil
	}
	server, err := NewServerWithSemantic(db, "test:1", "http://127.0.0.1:11434/", factory)
	if err != nil {
		t.Fatal(err)
	}
	client := semanticClient(t, server)
	tools, err = client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name == "brain_semantic" {
			found = tool.Annotations != nil && tool.Annotations.ReadOnlyHint
			encoded, err := json.Marshal(tool.InputSchema)
			if err != nil || strings.Contains(string(encoded), "endpoint") || strings.Contains(string(encoded), "model") {
				t.Fatalf("tool can replace owner configuration: %s %v", encoded, err)
			}
		}
	}
	if !found || calls.Load() != 0 {
		t.Fatal("tool discovery contacted provider or omitted read-only tool")
	}
	res, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "brain_semantic", Arguments: map[string]any{"query": "automobile " + secret, "scope": "project", "min_score": 0.5}})
	if err != nil || res.IsError {
		t.Fatalf("semantic call failed: %+v %v", res, err)
	}
	encoded, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out semantic.SearchResponse
	if err := json.Unmarshal(encoded, &out); err != nil || out.Status != "ready" || len(out.Memories) != 1 || out.Memories[0].ID != "m" || out.Model != "test:1" || calls.Load() != 1 || closed.Load() != 1 {
		t.Fatalf("wrong transport result: %s %v", encoded, err)
	}
	// Invalid inputs and endpoint overrides must not reach the service.
	for _, arguments := range []map[string]any{{"query": ""}, {"query": "car", "limit": 51}, {"query": "car", "endpoint": "http://127.0.0.2:1"}} {
		res, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "brain_semantic", Arguments: arguments})
		if err == nil && !res.IsError {
			t.Fatal("invalid/override request accepted")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("invalid request created provider")
	}
	if _, err := db.Exec("UPDATE memories SET content='source changed' WHERE id='m'"); err != nil {
		t.Fatal(err)
	}
	res, err = client.CallTool(context.Background(), &mcp.CallToolParams{Name: "brain_semantic", Arguments: map[string]any{"query": "automobile"}})
	if err != nil || res.IsError {
		t.Fatalf("stale call: %+v %v", res, err)
	}
	encoded, _ = json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(encoded, &out); err != nil || out.Status != "needs_embeddings" || out.StaleVectors != 1 || len(out.Memories) != 0 {
		t.Fatalf("stale memory escaped transport: %s %v", encoded, err)
	}
}

func TestRegressionSemanticMCPBoundsConcurrencyAndReleasesCancelledQueries(t *testing.T) {
	db := semanticDB(t)
	entered := make(chan struct{})
	var block atomic.Bool
	block.Store(true)
	var closed atomic.Int64
	options := &semanticOptions{model: "test:1", endpoint: "http://127.0.0.1:11434", gate: make(chan struct{}, 1), factory: func(string) (semantic.Provider, error) {
		return &mcpModel{closed: &closed, digest: func(ctx context.Context) (string, error) {
			if block.Load() {
				close(entered)
				<-ctx.Done()
				return "", ctx.Err()
			}
			return strings.Repeat("a", 64), ctx.Err()
		}}, nil
	}}
	handler := options.handler(db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := handler(ctx, nil, semanticInput{Query: "automobile"}); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("query did not enter provider")
	}
	if _, _, err := handler(context.Background(), nil, semanticInput{Query: "automobile"}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatal("overlapping inference was allowed", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || closed.Load() != 1 {
		t.Fatal("cancellation did not close provider", err)
	}
	block.Store(false)
	if _, out, err := handler(context.Background(), nil, semanticInput{Query: "automobile"}); err != nil || out.Status != "ready" || closed.Load() != 2 {
		t.Fatal("cancelled query retained gate", err)
	}
}

func TestRegressionSemanticMCPRejectsUnsafeConfigurationWithoutProvider(t *testing.T) {
	factory := func(string) (semantic.Provider, error) {
		t.Fatal("configuration validation invoked provider")
		return nil, nil
	}
	for _, endpoint := range []string{"https://example.com", "http://user:pass@localhost", "http://localhost/path", "http://localhost?key=value"} {
		if _, err := NewServerWithSemantic(nil, "test:1", endpoint, factory); err == nil {
			t.Fatal("unsafe endpoint accepted", endpoint)
		}
	}
	if _, err := NewServerWithSemantic(nil, "", "http://localhost", factory); err == nil {
		t.Fatal("empty model enabled learned search")
	}
}

func TestRegressionSemanticMCPDoesNotEchoPrivateBackendFailures(t *testing.T) {
	db := semanticDB(t)
	secret := "ghp_abcdefghijklmnopqrstuvwxyz"
	server, err := NewServerWithSemantic(db, "test:1", "http://127.0.0.1:11434", func(string) (semantic.Provider, error) {
		return &mcpModel{digest: func(context.Context) (string, error) { return "", fmt.Errorf("private backend failure: %s", secret) }}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	client := semanticClient(t, server)
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "brain_semantic", Arguments: map[string]any{"query": "automobile"}})
	if err != nil || !result.IsError {
		t.Fatalf("backend failure was not explicit: %+v %v", result, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), secret) || result.StructuredContent != nil {
		t.Fatalf("backend failure leaked content: %s %v", encoded, err)
	}
}
