package mcp

import (
	"context"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type memoryID struct {
	ID string `json:"id"`
}
type updateInput struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Scope   string `json:"scope,omitempty"`
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
