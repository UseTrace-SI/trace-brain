package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	brainmcp "github.com/UseTrace-SI/trace-brain/internal/mcp"
	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Query redaction once removed ordinary identifiers that follow a key-like
// word, so brief and brain_search missed results recall and context found.
// Every surface now searches the raw words; only echoed words are redacted.
func TestKeyContextQueriesReturnTheSameTopResultEverywhere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	contents := map[string]string{
		"factory": "token ProjectSettingsRepositoryImplementationFactory builds the settings repository",
		"bearer":  "authorization: bearer middleware validates request headers",
		"other":   "unrelated stylesheet colors",
	}
	for id, content := range contents {
		if err := store.Remember(ctx, db, id, "project", content, "manual", 1); err != nil {
			t.Fatal(err)
		}
	}
	session := mcpSession(t, db)
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		cmd := newRoot()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--db", path}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	for query, want := range map[string]string{
		"token ProjectSettingsRepositoryImplementationFactory": "factory",
		"authorization: bearer middleware":                     "bearer",
	} {
		if m := regexp.MustCompile(`^\[(\w+)\]`).FindStringSubmatch(run("recall", query)); m == nil || m[1] != want {
			t.Fatalf("recall %q: %v", query, m)
		}
		var context memoryContext
		if err := json.Unmarshal([]byte(run("context", "--trace-db", "", "--query", query)), &context); err != nil || len(context.Memories) == 0 || context.Memories[0].ID != want {
			t.Fatalf("context %q: %+v %v", query, context.Memories, err)
		}
		stored, err := store.Get(ctx, db, want)
		if err != nil {
			t.Fatal(err)
		}
		if lines := strings.Split(run("brief", query), "\n"); len(lines) < 2 || lines[1] != "- "+stored.Content {
			t.Fatalf("brief %q: %q", query, lines)
		}
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "brain_search", Arguments: map[string]any{"query": query}})
		if err != nil || res.IsError {
			t.Fatalf("brain_search %q: %v %v", query, err, res)
		}
		var out struct {
			Terms    []string `json:"terms"`
			Memories []struct {
				ID string `json:"id"`
			} `json:"memories"`
		}
		raw, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(raw, &out); err != nil || len(out.Memories) == 0 || out.Memories[0].ID != want {
			t.Fatalf("brain_search %q: %s %v", query, raw, err)
		}
	}
	// Echoed words never carry a secret, even though the raw words are searched.
	secret := "sk-proj-" + strings.Repeat("Ab3dE5fG7hJ9kL1mN2pQ", 3)
	if brief := run("brief", "rotate", "api_key="+secret); strings.Contains(brief, "Ab3dE5fG7hJ9kL1mN2pQ") || !strings.HasPrefix(brief, "Brief for: rotate api_key\n") {
		t.Fatalf("brief echoed a secret: %q", brief)
	}
}

func mcpSession(t *testing.T, sqlDB *sql.DB) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := brainmcp.NewServer(sqlDB).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close(); sqlDB.Close() })
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}
