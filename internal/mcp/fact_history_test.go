package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFactHistoryToolReturnsSupersededClaimsWithinBounds(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, m := range []struct {
		id, text string
		at       int64
	}{{"a", "Use Jest for tests.", 100}, {"b", "Use Vitest for tests.", 200}} {
		if err := store.Remember(ctx, db, m.id, "project", m.text, "manual", m.at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ExtractRules(ctx, db, store.RulesOptions{Limit: 10, At: 1000}); err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := NewServer(db).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	tools, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tools.Tools {
		found = found || (tool.Name == "brain_fact_history" && tool.Annotations != nil && tool.Annotations.ReadOnlyHint)
	}
	if !found {
		t.Fatal("brain_fact_history is not a read-only tool")
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := clientSession.CallTool(callCtx, &mcp.CallToolParams{Name: "brain_fact_history", Arguments: map[string]any{"subject": "tests"}})
	if err != nil || res.IsError {
		t.Fatalf("history call failed: %+v %v", res, err)
	}
	encoded, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out factHistoryOutput
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Result.Facts) != 2 || out.Result.Facts[0].Object != "jest" || out.Result.Facts[0].SupersededBy == nil || out.Result.Facts[0].SupersededBy.EvidenceID != "b" {
		t.Fatalf("history: %s", encoded)
	}
	if len(encoded) > maxFactHistoryBytes {
		t.Fatalf("payload exceeds bound: %d", len(encoded))
	}
	past, err := clientSession.CallTool(callCtx, &mcp.CallToolParams{Name: "brain_fact_history", Arguments: map[string]any{"subject": "tests", "known_at": 999}})
	if err != nil || past.IsError || !strings.Contains(textOf(past), `"facts":[]`) {
		t.Fatalf("history leaked later knowledge: %+v %v", past, err)
	}
	for _, arguments := range []map[string]any{{"subject": "tests", "limit": 51}, {"subject": ""}, {"subject": "tests", "known_at": -1}} {
		res, err := clientSession.CallTool(callCtx, &mcp.CallToolParams{Name: "brain_fact_history", Arguments: arguments})
		if err == nil && !res.IsError {
			t.Fatalf("accepted %v", arguments)
		}
	}
}

func textOf(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, content := range res.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}
