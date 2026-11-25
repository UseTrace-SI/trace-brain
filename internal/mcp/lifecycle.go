package mcp

import (
	"context"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type memoryID struct {
	ID string `json:"id" jsonschema:"memory ID as returned by brain_search or brain_remember"`
}
type updateInput struct {
	ID      string `json:"id" jsonschema:"ID of a manual or MCP memory"`
	Content string `json:"content" jsonschema:"replacement text (1–65536 bytes; secrets are redacted before storage)"`
	Scope   string `json:"scope,omitempty" jsonschema:"new scope; omit to keep the current scope"`
}
type updateOutput struct {
	ID      string `json:"id"`
	Updated bool   `json:"updated"`
}
type forgetOutput struct {
	ID              string `json:"id"`
	Forgotten       bool   `json:"forgotten"`
	SourceUnchanged bool   `json:"source_unchanged"`
}
type historyOutput struct {
	ID        string           `json:"id"`
	Revisions []store.Revision `json:"revisions"`
}

func (s *Server) get(ctx context.Context, _ *mcp.CallToolRequest, in memoryID) (*mcp.CallToolResult, store.Memory, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	m, err := store.Get(ctx, s.DB, in.ID)
	return nil, m, err
}
func (s *Server) update(ctx context.Context, _ *mcp.CallToolRequest, in updateInput) (*mcp.CallToolResult, updateOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err := store.Update(ctx, s.DB, in.ID, in.Scope, in.Content, time.Now().UnixMilli())
	return nil, updateOutput{ID: in.ID, Updated: err == nil}, err
}
func (s *Server) forget(ctx context.Context, _ *mcp.CallToolRequest, in memoryID) (*mcp.CallToolResult, forgetOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err := store.Forget(ctx, s.DB, in.ID)
	return nil, forgetOutput{ID: in.ID, Forgotten: err == nil, SourceUnchanged: true}, err
}
func (s *Server) history(ctx context.Context, _ *mcp.CallToolRequest, in memoryID) (*mcp.CallToolResult, historyOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := store.Get(ctx, s.DB, in.ID); err != nil {
		return nil, historyOutput{}, err
	}
	items, err := store.History(ctx, s.DB, in.ID)
	for i := range items {
		runes := []rune(items[i].Content)
		if len(runes) > 2048 {
			items[i].Content = string(runes[:2048])
			items[i].ContentTruncated = true
		}
	}
	return nil, historyOutput{ID: in.ID, Revisions: items}, err
}
