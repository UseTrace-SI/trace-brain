package mcp

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/semantic"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type semanticOptions struct {
	model, endpoint string
	factory         semantic.Factory
	gate            chan struct{}
}

// NewServerWithSemantic validates configuration without probing a service.
// Only the owner at server startup can select the endpoint and installed model.
func NewServerWithSemantic(db *sql.DB, model, endpoint string, factory semantic.Factory) (*mcp.Server, error) {
	if err := semantic.ValidateModel(model); err != nil {
		return nil, err
	}
	probe, err := semantic.NewOllama(endpoint)
	if err != nil {
		return nil, err
	}
	endpoint = probe.Origin()
	probe.Close()
	if factory == nil {
		factory = func(endpoint string) (semantic.Provider, error) { return semantic.NewOllama(endpoint) }
	}
	return newServer(db, &semanticOptions{model: model, endpoint: endpoint, factory: factory, gate: make(chan struct{}, 1)}), nil
}

type semanticInput struct {
	Query    string   `json:"query"`
	Scope    string   `json:"scope,omitempty"`
	Limit    int      `json:"limit,omitempty"`
	MinScore *float64 `json:"min_score,omitempty"`
}

func (options *semanticOptions) handler(db *sql.DB) func(context.Context, *mcp.CallToolRequest, semanticInput) (*mcp.CallToolResult, semantic.SearchResponse, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in semanticInput) (*mcp.CallToolResult, semantic.SearchResponse, error) {
		request := semantic.SearchRequest{Query: in.Query, Scope: in.Scope, Model: options.model, Limit: in.Limit, MinScore: -1}
		if request.Limit == 0 {
			request.Limit = 20
		}
		if in.MinScore != nil {
			request.MinScore = *in.MinScore
		}
		if err := semantic.ValidateSearch(request); err != nil {
			return nil, semantic.SearchResponse{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if err := ctx.Err(); err != nil {
			return nil, semantic.SearchResponse{}, err
		}
		select {
		case options.gate <- struct{}{}:
			defer func() { <-options.gate }()
		default:
			return nil, semantic.SearchResponse{}, fmt.Errorf("a semantic query is already running; retry after it completes")
		}
		provider, err := options.factory(options.endpoint)
		if err != nil {
			return nil, semantic.SearchResponse{}, fmt.Errorf("local semantic provider unavailable")
		}
		defer provider.Close()
		if provider.Origin() != options.endpoint {
			return nil, semantic.SearchResponse{}, fmt.Errorf("semantic provider origin differs from server configuration")
		}
		out, err := semantic.Search(ctx, db, provider, request)
		if err != nil {
			if ctx.Err() != nil {
				return nil, semantic.SearchResponse{}, fmt.Errorf("local semantic query cancelled: %w", ctx.Err())
			}
			// A provider or malformed legacy DB can include private values in an
			// error. Do not forward those values through the agent protocol.
			return nil, semantic.SearchResponse{}, fmt.Errorf("local semantic query failed; verify the selected local model and existing embeddings")
		}
		return nil, out, nil
	}
}
