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
}
