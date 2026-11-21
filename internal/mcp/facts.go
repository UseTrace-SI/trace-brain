package mcp

import (
	"context"
	"encoding/json"
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
	EvidenceMode string `json:"evidence_mode,omitempty"`
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
	result, err := store.QueryFacts(ctx, s.DB, store.FactQuery{Scope: input.Scope, Subject: input.Subject, Predicate: input.Predicate, Object: input.Object, At: at, KnownAt: known, Limit: input.Limit, IncludeStale: input.IncludeStale, EvidenceMode: input.EvidenceMode})
	return nil, factQueryOutput{Result: result, At: at, KnownAt: known, EvidenceCheck: store.FactEvidenceCheck(result.EvidenceMode)}, err
}

type factEvidenceInput struct {
	ID      string `json:"id"`
	Scope   string `json:"scope,omitempty"`
	KnownAt *int64 `json:"known_at,omitempty"`
}
type factEvidenceOutput struct {
	Evidence         store.FactEvidence `json:"evidence"`
	KnownAt          int64              `json:"known_at"`
	ContentBytes     int                `json:"content_bytes"`
	ContentTruncated bool               `json:"content_truncated"`
}

func (s *Server) factEvidence(ctx context.Context, _ *mcp.CallToolRequest, input factEvidenceInput) (*mcp.CallToolResult, factEvidenceOutput, error) {
	if input.Scope == "" {
		input.Scope = "project"
	}
	known := time.Now().UnixMilli()
	if input.KnownAt != nil {
		known = *input.KnownAt
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	evidence, err := store.GetFactEvidence(ctx, s.DB, input.ID, input.Scope, known)
	if err != nil {
		return nil, factEvidenceOutput{}, err
	}
	content, bytes, truncated := store.CapFactEvidenceContent(evidence.Content)
	evidence.Content = content
	return nil, factEvidenceOutput{Evidence: evidence, KnownAt: known, ContentBytes: bytes, ContentTruncated: truncated}, nil
}

type factHistoryInput struct {
	Scope     string `json:"scope,omitempty"`
	Subject   string `json:"subject"`
	Predicate string `json:"predicate,omitempty"`
	KnownAt   *int64 `json:"known_at,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}
type factHistoryOutput struct {
	Result        store.FactResults `json:"result"`
	KnownAt       int64             `json:"known_at"`
	Scope         string            `json:"scope"`
	EvidenceCheck string            `json:"evidence_check"`
}

// maxFactHistoryBytes bounds the encoded brain_fact_history payload.
const maxFactHistoryBytes = 64 << 10

// factHistory returns every claim about one subject known at known_at,
// including values later superseded, with their effective validity.
func (s *Server) factHistory(ctx context.Context, _ *mcp.CallToolRequest, input factHistoryInput) (*mcp.CallToolResult, factHistoryOutput, error) {
	if input.Limit == 0 {
		input.Limit = 20
	}
	if input.Limit < 1 || input.Limit > 50 {
		return nil, factHistoryOutput{}, fmt.Errorf("fact history limit must be 1–50")
	}
	if input.Scope == "" {
		input.Scope = "project"
	}
	known := time.Now().UnixMilli()
	if input.KnownAt != nil {
		known = *input.KnownAt
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	result, err := store.FactTimeline(ctx, s.DB, store.TimelineQuery{Scope: input.Scope, Subject: input.Subject, Predicate: input.Predicate, KnownAt: known, Limit: input.Limit})
	if err != nil {
		return nil, factHistoryOutput{}, err
	}
	for i := range result.Facts {
		f := &result.Facts[i]
		f.Subject, f.Object, f.Quote = redactText(f.Subject), redactText(f.Object), redactText(f.Quote)
	}
	out := factHistoryOutput{Result: result, KnownAt: known, Scope: input.Scope, EvidenceCheck: "current_memory_hash_or_quote"}
	for {
		encoded, err := json.Marshal(out)
		if err != nil {
			return nil, factHistoryOutput{}, err
		}
		if len(encoded) <= maxFactHistoryBytes || len(out.Result.Facts) == 0 {
			return nil, out, nil
		}
		out.Result.Facts = out.Result.Facts[:len(out.Result.Facts)-1]
		out.Result.Truncated = true
	}
}
