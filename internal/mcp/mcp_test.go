package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// runtimeCredential builds a credential-shaped value at runtime so the source
// tree contains no literal that push protection would flag.
func runtimeCredential() string {
	const alphabet = "Ab3Cd5Ef7Gh9Jk1Lm2Np4Qr6"
	value := make([]byte, 40)
	for i := range value {
		value[i] = alphabet[i%len(alphabet)]
	}
	return string(value)
}

func TestRegressionMCPMemoryToolsEnforceOwnDeadlines(t *testing.T) {
	// Shorten the tool deadline so this regression stays fast even when CI
	// repeats it 100 times. A parent deadline well above it distinguishes the
	// tool's own bound from caller cancellation.
	original := mcpToolTimeout
	mcpToolTimeout = 50 * time.Millisecond
	defer func() { mcpToolTimeout = original }()

	db, err := store.Open(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Occupy the only pooled connection so any tool query must wait until its
	// own context deadline expires instead of SQLite's 30s busy timeout.
	db.SetMaxOpenConns(1)
	holder, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer holder.ExecContext(context.Background(), "ROLLBACK")

	s := New(db)
	parent, cancel := context.WithTimeout(context.Background(), 3*time.Second*raceSlowdown)
	defer cancel()
	for name, call := range map[string]func(context.Context) error{
		"brain_remember": func(ctx context.Context) error {
			_, _, err := s.remember(ctx, nil, rememberInput{Content: "x"})
			return err
		},
		"brain_recall": func(ctx context.Context) error { _, _, err := s.recall(ctx, nil, recallInput{Query: "x"}); return err },
		"brain_query":  func(ctx context.Context) error { _, _, err := s.query(ctx, nil, queryInput{Query: "x"}); return err },
		"brain_brief":  func(ctx context.Context) error { _, _, err := s.brief(ctx, nil, briefInput{Task: "x"}); return err },
		"brain_search": func(ctx context.Context) error { _, _, err := s.search(ctx, nil, searchInput{Query: "x"}); return err },
		"brain_status": func(ctx context.Context) error { _, _, err := s.status(ctx, nil, struct{}{}); return err },
		"brain_overview": func(ctx context.Context) error {
			_, _, err := s.overview(ctx, nil, struct{}{})
			return err
		},
	} {
		start := time.Now()
		err := call(parent)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s did not enforce its own deadline: %v", name, err)
		}
		if elapsed := time.Since(start); elapsed > time.Second*raceSlowdown { // far below the 30 s busy timeout
			t.Fatalf("%s stalled for %s instead of its own deadline", name, elapsed)
		}
	}
}

func TestRegressionRecallQueryAndBriefReapplyRedactionOnRead(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	secret := runtimeCredential()
	ctx := context.Background()
	// Direct insert emulates a legacy/corrupted row that escaped write-time
	// redaction. Read paths must re-apply the policy like Get does.
	if _, err := db.Exec("INSERT INTO memories(id,scope,content,source,created_at) VALUES('m',?,?,'manual',1)", "scope "+secret, "lexicalterm "+secret); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	for name, run := range map[string]func() (string, error){
		"recall": func() (string, error) {
			_, out, err := s.recall(ctx, nil, recallInput{Query: "lexicalterm"})
			if err != nil {
				return "", err
			}
			b, err := json.Marshal(out)
			return string(b), err
		},
		"query": func() (string, error) {
			_, out, err := s.query(ctx, nil, queryInput{Query: "lexicalterm"})
			if err != nil {
				return "", err
			}
			b, err := json.Marshal(out)
			return string(b), err
		},
		"brief": func() (string, error) {
			_, out, err := s.brief(ctx, nil, briefInput{Task: "lexicalterm"})
			if err != nil {
				return "", err
			}
			b, err := json.Marshal(out)
			return string(b), err
		},
	} {
		got, err := run()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(got, secret) || !strings.Contains(got, "redacted:") {
			t.Fatalf("%s leaked unredacted stored text: %s", name, got)
		}
	}
}

// A learned semantic scan holds a read connection while scanning up to 100k
// rows. Other MCP tools must not starve on a single-connection pool. Holding a
// read cursor open here simulates that scan; brain_remember must still commit
// promptly (WAL readers do not block writers).
func TestConcurrentSemanticScanDoesNotBlockBrainRemember(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := store.Remember(ctx, db, "seed", "project", "seed lexicalterm", "manual", 1); err != nil {
		t.Fatal(err)
	}
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

	rows, err := db.QueryContext(ctx, `SELECT substr(m.content,1,65537) FROM memories m LEFT JOIN semantic_embeddings e ON e.memory_id=m.id ORDER BY m.id LIMIT 100001`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("expected a seed row for the simulated scan")
	}

	writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second*raceSlowdown)
	defer cancel()
	res, err := clientSession.CallTool(writeCtx, &mcp.CallToolParams{Name: "brain_remember", Arguments: map[string]any{"content": "written during scan"}})
	if err != nil || res.IsError {
		t.Fatalf("brain_remember blocked by a held semantic read: %v %v", err, res)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM memories WHERE content LIKE '%written during scan%'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("remember during scan not persisted: %d %v", n, err)
	}
}

// Empty recall/query results must serialize as [] rather than null, and a
// row-level scan failure in brief must not be swallowed.
func TestEmptyRecallAndQuerySerializeAsEmptyArray(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db)
	for _, call := range []struct {
		name string
		fn   func(context.Context, *mcp.CallToolRequest) (any, error)
	}{
		{"recall", func(ctx context.Context, r *mcp.CallToolRequest) (any, error) {
			_, out, err := s.recall(ctx, r, recallInput{Query: "nothing"})
			return out, err
		}},
		{"query", func(ctx context.Context, r *mcp.CallToolRequest) (any, error) {
			_, out, err := s.query(ctx, r, queryInput{Query: "nothing"})
			return out, err
		}},
	} {
		out, err := call.fn(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"memories":[]`) {
			t.Fatalf("%s empty result serialized as %s", call.name, data)
		}
	}
}

func TestBriefDoesNotIgnoreScanErrors(t *testing.T) {
	db, err := sql.Open("sqlite", t.TempDir()+"/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE memories(id TEXT PRIMARY KEY, scope TEXT, content TEXT, source TEXT, session_id TEXT, turn_id TEXT, created_at INTEGER);
		CREATE VIRTUAL TABLE memory_fts USING fts5(content, content='memories', content_rowid='rowid');
		CREATE TRIGGER memories_ai AFTER INSERT ON memories BEGIN INSERT INTO memory_fts(rowid,content) VALUES(new.rowid,new.content); END;
		INSERT INTO memories(id,scope,content,created_at) VALUES('m','project','matching lexicalterm',1);
		UPDATE memories SET content=NULL`); err != nil {
		t.Fatal(err)
	}
	// substr(NULL,...) yields NULL, which cannot be scanned into a string.
	// Previously brief dropped such rows and reported success.
	if _, _, err := New(db).brief(context.Background(), nil, briefInput{Task: "lexicalterm"}); err == nil {
		t.Fatal("brief ignored a row scan error")
	}
}

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
	var id string
	if err := db.QueryRow("SELECT id FROM memories LIMIT 1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	fact, err := store.AssertFact(ctx, db, store.Fact{Scope: "project", Subject: "client", Predicate: "retries", Object: "three", EvidenceID: id, ValidFrom: 0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	res, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "brain_facts", Arguments: map[string]any{"subject": "client", "limit": 10}})
	if err != nil || res.IsError {
		t.Fatalf("facts failed: %v %v", err, res)
	}
	for _, step := range []struct {
		name string
		args map[string]any
	}{
		{"brain_get", map[string]any{"id": id}},
		{"brain_update", map[string]any{"id": id, "content": "updated retry policy"}},
		{"brain_history", map[string]any{"id": id}},
		{"brain_forget", map[string]any{"id": id}},
	} {
		res, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: step.name, Arguments: step.args})
		if err != nil || res.IsError {
			t.Fatalf("%s failed: %v %v", step.name, err, res)
		}
		if step.name == "brain_update" {
			res, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "brain_fact_evidence", Arguments: map[string]any{"id": fact.ID, "known_at": 1}})
			if err != nil || res.IsError {
				t.Fatalf("captured evidence failed over MCP transport: %v %v", err, res)
			}
			res, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "brain_facts", Arguments: map[string]any{"subject": "client", "evidence_mode": "captured", "known_at": 1}})
			if err != nil || res.IsError {
				t.Fatalf("captured fact query failed over MCP transport: %v %v", err, res)
			}
		}
	}
	res, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "brain_fact_evidence", Arguments: map[string]any{"id": fact.ID}})
	if err == nil && !res.IsError {
		t.Fatal("forgotten captured evidence returned over MCP")
	}
	var remaining int
	if err := db.QueryRow("SELECT count(*) FROM memory_revisions").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("forgotten revisions remain: %d %v", remaining, err)
	}
}
