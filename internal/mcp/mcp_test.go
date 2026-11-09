package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMemoryTools(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := NewServer(db).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	// remember
	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "brain_remember", Arguments: map[string]any{"content": "retry failed requests at most three times"}})
	if err != nil || res.IsError {
		t.Fatalf("remember failed: %v %v", err, res)
	}
	// recall
	res, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "brain_recall", Arguments: map[string]any{"query": "retry"}})
	if err != nil || res.IsError {
		t.Fatalf("recall failed: %v %v", err, res)
	}
	// status
	res, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "brain_status", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("status failed: %v %v", err, res)
	}
	// overview
	res, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "brain_overview", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("overview failed: %v %v", err, res)
	}
	// brief
	res, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "brain_brief", Arguments: map[string]any{"task": "retry"}})
	if err != nil || res.IsError {
		t.Fatalf("brief failed: %v %v", err, res)
	}
	// query
	res, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "brain_query", Arguments: map[string]any{"query": "retry"}})
	if err != nil || res.IsError {
		t.Fatalf("query failed: %v %v", err, res)
	}
	var id string
	if err := db.QueryRow("SELECT id FROM memories LIMIT 1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	fact, err := store.AssertFact(ctx, db, store.Fact{Scope: "project", Subject: "client", Predicate: "retries", Object: "three", EvidenceID: id, ValidFrom: 0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	res, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "brain_facts", Arguments: map[string]any{"subject": "client", "limit": 10}})
	if err != nil || res.IsError {
		t.Fatalf("facts failed: %v %v", err, res)
	}
	for _, step := range []struct {
		name string
		args map[string]any
	}{
		{"brain_get", map[string]any{"id": id}},
		{"brain_update", map[string]any{"id": id, "content": "updated retry policy"}},
		{"brain_history", map[string]any{"id": id}},
		{"brain_forget", map[string]any{"id": id}},
	} {
		res, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: step.name, Arguments: step.args})
		if err != nil || res.IsError {
			t.Fatalf("%s failed: %v %v", step.name, err, res)
		}
		if step.name == "brain_update" {
			res, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "brain_fact_evidence", Arguments: map[string]any{"id": fact.ID, "known_at": 1}})
			if err != nil || res.IsError {
				t.Fatalf("captured evidence failed over MCP transport: %v %v", err, res)
			}
			res, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "brain_facts", Arguments: map[string]any{"subject": "client", "evidence_mode": "captured", "known_at": 1}})
			if err != nil || res.IsError {
				t.Fatalf("captured fact query failed over MCP transport: %v %v", err, res)
			}
		}
	}
	res, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "brain_fact_evidence", Arguments: map[string]any{"id": fact.ID}})
	if err == nil && !res.IsError {
		t.Fatal("forgotten captured evidence returned over MCP")
	}
	var remaining int
	if err := db.QueryRow("SELECT count(*) FROM memory_revisions").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("forgotten revisions remain: %d %v", remaining, err)
	}
}
