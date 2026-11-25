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

func connectTestClient(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestBrainSearchReturnsRankedMemoriesWithProvenance(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	ctx := context.Background()
	if err := store.Remember(ctx, db, "manual-note", "project", "We decided the tests are flaky because of shared temp dirs", "manual", now.Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memories(id,scope,content,source,session_id,turn_id,created_at) VALUES
		('turn','turns','Turn t: fixing flaky test in CI','trace-import','s1','t1',?),
		('old','turns','tests were flaky last year','trace-import','s0','t0',?),
		('other','docs','the cat sat on the mat','document','','',?)`,
		now.Add(-2*time.Hour).UnixMilli(), now.Add(-400*24*time.Hour).UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	client := connectTestClient(t, NewServer(db))
	call := func(args map[string]any) (searchOutput, *mcp.CallToolResult) {
		t.Helper()
		res, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "brain_search", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		var out searchOutput
		if !res.IsError {
			raw, _ := json.Marshal(res.StructuredContent)
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatal(err)
			}
		}
		return out, res
	}
	out, _ := call(map[string]any{"query": "the flaky test"})
	if strings.Join(out.Terms, ",") != "flaky,test" || len(out.Memories) != 3 {
		t.Fatalf("search: %+v", out)
	}
	var turn *searchMemory
	for i, m := range out.Memories {
		if m.Score <= 0 || (i > 0 && out.Memories[i-1].Score < m.Score) {
			t.Fatalf("scores not ranked: %+v", out.Memories)
		}
		if m.ID == "turn" {
			turn = &out.Memories[i]
		}
	}
	if turn == nil || turn.Source != "trace-import" || turn.SessionID != "s1" || turn.TurnID != "t1" || turn.Scope != "turns" || turn.CreatedAt == 0 {
		t.Fatalf("provenance missing: %+v", out.Memories)
	}
	if out.Memories[len(out.Memories)-1].ID != "old" {
		t.Fatalf("year-old match should rank last: %+v", out.Memories)
	}
	if out, _ = call(map[string]any{"query": "flaky", "since": "7d"}); len(out.Memories) != 2 || out.Since == 0 {
		t.Fatalf("relative since: %+v", out)
	}
	since := now.Add(-90 * time.Minute).UTC().Format(time.RFC3339)
	if out, _ = call(map[string]any{"query": "flaky", "since": since}); len(out.Memories) != 1 || out.Memories[0].ID != "manual-note" {
		t.Fatalf("absolute since: %+v", out)
	}
	if out, _ = call(map[string]any{"query": "flaky", "scope": "turns", "limit": 1}); len(out.Memories) != 1 || out.Memories[0].Scope != "turns" {
		t.Fatalf("scope/limit: %+v", out)
	}
	if out, _ = call(map[string]any{"since": "3h"}); len(out.Memories) != 3 || out.Memories[0].ID != "other" {
		t.Fatalf("recent listing: %+v", out)
	}
	for _, bad := range []map[string]any{{"query": "x", "since": "yesterday"}, {"query": "x", "since": "-5h"}, {"query": "x", "limit": 51}, {"query": strings.Repeat("x", 4097)}} {
		if _, res := call(bad); !res.IsError {
			t.Fatalf("accepted %v", bad)
		}
	}
}

func TestToolsAreDescribedAnnotatedAndVersioned(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	original := Version
	Version = "v9.9.9-test"
	defer func() { Version = original }()
	client := connectTestClient(t, NewServer(db))
	if got := client.InitializeResult().ServerInfo.Version; got != "v9.9.9-test" {
		t.Fatalf("server version %q", got)
	}
	tools, err := client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	readOnly := map[string]bool{"brain_search": true, "brain_recall": true, "brain_query": true, "brain_brief": true, "brain_status": true, "brain_overview": true,
		"brain_get": true, "brain_history": true, "brain_facts": true, "brain_fact_history": true, "brain_fact_evidence": true}
	seen := map[string]bool{}
	for _, tool := range tools.Tools {
		seen[tool.Name] = true
		if len(tool.Description) < 40 {
			t.Errorf("%s has no real description: %q", tool.Name, tool.Description)
		}
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint != readOnly[tool.Name] {
			t.Errorf("%s read-only hint wrong: %+v", tool.Name, tool.Annotations)
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		for name, property := range schema.Properties {
			if property.Description == "" {
				t.Errorf("%s.%s has no description", tool.Name, name)
			}
		}
	}
	// Legacy names stay registered for existing clients.
	for _, name := range []string{"brain_search", "brain_recall", "brain_query", "brain_status", "brain_overview", "brain_brief", "brain_remember"} {
		if !seen[name] {
			t.Errorf("%s not registered", name)
		}
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	for text, want := range map[string]time.Time{
		"":                     {},
		"90m":                  now.Add(-90 * time.Minute),
		"36h":                  now.Add(-36 * time.Hour),
		"7d":                   now.Add(-7 * 24 * time.Hour),
		"1.5d":                 now.Add(-36 * time.Hour),
		"2026-01-01T00:00:00Z": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		got, err := parseSince(text, now)
		expected := int64(0)
		if !want.IsZero() {
			expected = want.UnixMilli()
		}
		if err != nil || got != expected {
			t.Fatalf("%q: %d %v want %d", text, got, err, expected)
		}
	}
	for _, text := range []string{"soon", "-1h", "d", "-2d", "1000000d", "9999999h", "NaNd", "nand", "+Infd", "-Infd", "Infd"} {
		if _, err := parseSince(text, now); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
}

// Terms are echoed to the client, so the query is redacted before it is
// split: per-word redaction let fragments of a split secret through.
func TestBrainSearchDoesNotEchoSecretFragments(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := store.Remember(context.Background(), db, "m", "project", "rotate the deploy credentials", "manual", 1); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	secret := "sk-proj-" + strings.Repeat("Ab3dE5fG7hJ9kL1mN2pQ", 3)
	for _, query := range []string{"deploy api_key=" + secret, "deploy " + secret} {
		_, out, err := s.search(context.Background(), nil, searchInput{Query: query})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), "Ab3dE5fG7hJ9kL1mN2pQ") {
			t.Fatalf("query %q echoed the secret: %s", query, raw)
		}
		for _, term := range out.Terms {
			if term == "sk" || term == "proj" || strings.Contains(term, "Ab3d") {
				t.Fatalf("query %q echoed secret fragment %q: %v", query, term, out.Terms)
			}
		}
		if len(out.Memories) != 1 || out.Terms[0] != "deploy" {
			t.Fatalf("search on the remaining words failed: %+v", out)
		}
	}
	// A query that is only a secret searches nothing rather than listing
	// recent memories.
	_, out, err := s.search(context.Background(), nil, searchInput{Query: secret})
	if err != nil || len(out.Memories) != 0 || len(out.Terms) != 0 {
		t.Fatalf("secret-only query: %+v %v", out, err)
	}
}
