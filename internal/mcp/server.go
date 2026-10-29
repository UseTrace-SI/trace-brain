package mcp

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Server struct {
	DB *sql.DB
}

func New(db *sql.DB) *Server { return &Server{DB: db} }
func NewServer(db *sql.DB) *mcp.Server {
	s := New(db)
	server := mcp.NewServer(&mcp.Implementation{Name: "trace-brain", Version: "dev"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "brain_remember", Description: "Save a memory"}, s.remember)
	mcp.AddTool(server, &mcp.Tool{Name: "brain_recall", Description: "Recall memories matching a query"}, s.recall)
	mcp.AddTool(server, &mcp.Tool{Name: "brain_status", Description: "Memory status"}, s.status)
	mcp.AddTool(server, &mcp.Tool{Name: "brain_query", Description: "Query memories"}, s.query)
	mcp.AddTool(server, &mcp.Tool{Name: "brain_brief", Description: "Brief for a task"}, s.brief)
	mcp.AddTool(server, &mcp.Tool{Name: "brain_overview", Description: "Project overview"}, s.overview)
	return server
}

func (s *Server) Run(ctx context.Context) error {
	return NewServer(s.DB).Run(ctx, &mcp.StdioTransport{})
}

type rememberInput struct {
	Content string `json:"content"`
	Scope   string `json:"scope,omitempty"`
}
type rememberOutput struct {
	ID string `json:"id"`
}

func (s *Server) remember(ctx context.Context, req *mcp.CallToolRequest, in rememberInput) (*mcp.CallToolResult, rememberOutput, error) {
	if in.Content == "" {
		return nil, rememberOutput{}, fmt.Errorf("content required")
	}
	scope := in.Scope
	if scope == "" {
		scope = "project"
	}
	id := fmt.Sprintf("mem-%d", time.Now().UnixNano())
	_, err := s.DB.Exec("INSERT INTO memories(id,scope,content,source,created_at) VALUES(?,?,?,?,?)",
		id, scope, in.Content, "mcp", time.Now().UnixMilli())
	if err != nil {
		return nil, rememberOutput{}, err
	}
	return nil, rememberOutput{ID: id}, nil
}

type recallInput struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}
type recallOutput struct {
	Memories []map[string]any `json:"memories"`
}

func (s *Server) recall(ctx context.Context, req *mcp.CallToolRequest, in recallInput) (*mcp.CallToolResult, recallOutput, error) {
	if in.Limit < 1 {
		in.Limit = 20
	}
	rows, err := s.DB.Query(`SELECT m.id,m.scope,m.content,m.created_at FROM memories m
		JOIN memory_fts f ON f.rowid=m.rowid WHERE memory_fts MATCH ? ORDER BY m.created_at DESC LIMIT ?`, in.Query, in.Limit)
	if err != nil {
		return nil, recallOutput{}, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, scope, content string
		var created int64
		if err := rows.Scan(&id, &scope, &content, &created); err != nil {
			return nil, recallOutput{}, err
		}
		out = append(out, map[string]any{"id": id, "scope": scope, "content": content, "created_at": created})
	}
	return nil, recallOutput{Memories: out}, rows.Err()
}



type queryInput struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}
type queryOutput struct {
	Memories []map[string]any `json:"memories"`
}
func (s *Server) query(ctx context.Context, req *mcp.CallToolRequest, in queryInput) (*mcp.CallToolResult, queryOutput, error) {
	if in.Limit < 1 { in.Limit = 20 }
	rows, err := s.DB.Query(`SELECT m.id,m.scope,m.content,m.created_at FROM memories m JOIN memory_fts f ON f.rowid=m.rowid WHERE memory_fts MATCH ? ORDER BY m.created_at DESC LIMIT ?`, in.Query, in.Limit)
	if err != nil { return nil, queryOutput{}, err }
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, scope, content string
		var created int64
		if err := rows.Scan(&id, &scope, &content, &created); err != nil { return nil, queryOutput{}, err }
		out = append(out, map[string]any{"id": id, "scope": scope, "content": content, "created_at": created})
	}
	return nil, queryOutput{Memories: out}, rows.Err()
}

type briefInput struct {
	Task string `json:"task"`
}
type briefOutput struct {
	Items []string `json:"items"`
}
func (s *Server) brief(ctx context.Context, req *mcp.CallToolRequest, in briefInput) (*mcp.CallToolResult, briefOutput, error) {
	rows, err := s.DB.Query(`SELECT m.content FROM memories m JOIN memory_fts f ON f.rowid=m.rowid WHERE memory_fts MATCH ? LIMIT 10`, in.Task)
	if err != nil { return nil, briefOutput{}, err }
	defer rows.Close()
	var items []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err == nil { items = append(items, c) }
	}
	return nil, briefOutput{Items: items}, rows.Err()
}

type overviewOutput struct {
	Memories int `json:"memories"`
}
func (s *Server) overview(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, overviewOutput, error) {
	var n int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n); err != nil {
		return nil, overviewOutput{}, err
	}
	return nil, overviewOutput{Memories: n}, nil
}


type statusOutput struct {
	Memories int `json:"memories"`
}

func (s *Server) status(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, statusOutput, error) {
	var n int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n); err != nil {
		return nil, statusOutput{}, err
	}
	return nil, statusOutput{Memories: n}, nil
}

