package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type factQueryInput struct {
	Scope        string `json:"scope,omitempty"`
	Subject      string `json:"subject,omitempty"`
	Predicate    string `json:"predicate,omitempty"`
	Object       string `json:"object,omitempty"`
	At           *int64 `json:"at,omitempty"`
	KnownAt      *int64 `json:"known_at,omitempty"`
	Limit        int    `json:"limit,omitempty"`
	IncludeStale bool   `json:"include_stale,omitempty"`
}
type factQueryOutput struct {
	Result        store.FactResults `json:"result"`
	At            int64             `json:"at"`
	KnownAt       int64             `json:"known_at"`
	EvidenceCheck string            `json:"evidence_check"`
}

func (s *Server) facts(ctx context.Context, _ *mcp.CallToolRequest, input factQueryInput) (*mcp.CallToolResult, factQueryOutput, error) {
	if input.Limit == 0 {
		input.Limit = 20
	}
	if input.Limit < 1 || input.Limit > 50 {
		return nil, factQueryOutput{}, fmt.Errorf("fact result limit must be 1–50")
	}
	if input.Scope == "" {
		input.Scope = "project"
	}
	now := time.Now().UnixMilli()
	at, known := now, now
	if input.At != nil {
		at = *input.At
	}
	if input.KnownAt != nil {
		known = *input.KnownAt
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	result, err := store.QueryFacts(ctx, s.DB, store.FactQuery{Scope: input.Scope, Subject: input.Subject, Predicate: input.Predicate, Object: input.Object, At: at, KnownAt: known, Limit: input.Limit, IncludeStale: input.IncludeStale})
	return nil, factQueryOutput{Result: result, At: at, KnownAt: known, EvidenceCheck: "current_memory_hash"}, err
}
