package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type searchInput struct {
	Query string `json:"query,omitempty" jsonschema:"words to search for, matched literally with English stemming; empty lists the most recent memories"`
	Scope string `json:"scope,omitempty" jsonschema:"exact memory scope filter, for example project or sessions"`
	Since string `json:"since,omitempty" jsonschema:"only memories created at or after this RFC3339 time or within this relative age (for example 90m, 36h or 7d)"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum memories to return (1–50, default 20)"`
}

// searchMemory is one ranked memory with its provenance.
type searchMemory struct {
	ID               string  `json:"id"`
	Scope            string  `json:"scope"`
	Content          string  `json:"content"`
	ContentTruncated bool    `json:"content_truncated"`
	Source           string  `json:"source"`
	SessionID        string  `json:"session_id,omitempty"`
	TurnID           string  `json:"turn_id,omitempty"`
	CreatedAt        int64   `json:"created_at"`
	Score            float64 `json:"score"`
}

type searchOutput struct {
	// Terms are the searched words (after stopword removal) that also occur
	// in the query redacted as a whole; words the redactor removes are
	// searched but not echoed.
	Terms    []string       `json:"terms"`
	Since    int64          `json:"since,omitempty"`
	Memories []searchMemory `json:"memories"`
}

// maxSinceAge bounds relative ages so the subtraction cannot overflow.
const maxSinceAge = 100 * 365 * 24 * time.Hour

// parseSince accepts an RFC3339 time or a relative age in Go duration syntax
// with an additional d (days) unit, and returns Unix milliseconds.
func parseSince(text string, now time.Time) (int64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, nil
	}
	if at, err := time.Parse(time.RFC3339, text); err == nil {
		return at.UnixMilli(), nil
	}
	var age time.Duration
	if days, ok := strings.CutSuffix(text, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		// Written so NaN fails the range check too.
		if err != nil || !(n >= 0 && n <= maxSinceAge.Hours()/24) {
			return 0, fmt.Errorf("since must be an RFC3339 time or a relative age such as 90m, 36h or 7d")
		}
		age = time.Duration(n * float64(24*time.Hour))
	} else {
		var err error
		if age, err = time.ParseDuration(text); err != nil || age < 0 || age > maxSinceAge {
			return 0, fmt.Errorf("since must be an RFC3339 time or a relative age such as 90m, 36h or 7d")
		}
	}
	return now.Add(-age).UnixMilli(), nil
}

func (s *Server) search(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (result *mcp.CallToolResult, out searchOutput, err error) {
	ctx, finish := withToolDeadline(ctx, "brain_search")
	defer func() { err = finish(err) }()
	if in.Limit == 0 {
		in.Limit = store.DefaultSearchLimit
	}
	if in.Limit < 1 || in.Limit > store.MaxSearchLimit {
		return nil, searchOutput{}, fmt.Errorf("limit must be 1–%d", store.MaxSearchLimit)
	}
	if len(in.Scope) > 1024 {
		return nil, searchOutput{}, fmt.Errorf("scope exceeds 1024 bytes")
	}
	now := time.Now()
	since, err := parseSince(in.Since, now)
	if err != nil {
		return nil, searchOutput{}, err
	}
	// Search uses the raw words (the query is local and never stored); only
	// the echoed terms are built from the query redacted as a whole, so they
	// never carry a fragment of a secret.
	if _, err := store.QueryTerms(in.Query); err != nil {
		return nil, searchOutput{}, err
	}
	terms := store.DisplayTerms(in.Query)
	results, err := store.Search(ctx, s.DB, store.SearchOpts{Query: in.Query, Scope: in.Scope, Since: since, Limit: in.Limit, RecentWhenEmpty: true, Now: now})
	if err != nil {
		return nil, searchOutput{}, err
	}
	out = searchOutput{Terms: terms, Since: since, Memories: []searchMemory{}}
	for _, m := range results {
		out.Memories = append(out.Memories, searchMemory{ID: m.ID, Scope: m.Scope, Content: m.Content, ContentTruncated: m.ContentTruncated, Source: m.Source, SessionID: m.SessionID, TurnID: m.TurnID, CreatedAt: m.CreatedAt, Score: m.Score})
	}
	return nil, out, nil
}
