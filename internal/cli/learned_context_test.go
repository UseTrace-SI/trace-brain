package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-brain/internal/embed"
	learned "github.com/UseTrace-SI/trace-brain/internal/semantic"
	"github.com/UseTrace-SI/trace-brain/internal/store"
)

func TestRegressionLearnedContextScopesHistoryAndRequiresDocumentConsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.db")
	sourceA, sourceB := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db")
	for _, file := range []string{sourceA, sourceB} {
		if err := os.WriteFile(file, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	sourceA, err = filepath.EvalSymlinks(sourceA)
	if err != nil {
		t.Fatal(err)
	}
	sourceB, err = filepath.EvalSymlinks(sourceB)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, item := range []struct {
		id, kind, scope, source string
		at                      int64
	}{
		{"own-trace", "trace-import", "project", sourceA, 1},
		{"foreign-trace", "trace-import", "project", sourceB, 99},
		{"manual", "manual", "project", "", 3},
		{"mcp", "mcp", "project", "", 2},
		{"local-doc", "document", "documents", "", 4},
	} {
		text := "a car has four wheels"
		if err := store.Remember(context.Background(), db, item.id, item.scope, text, item.kind, item.at); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO semantic_embeddings VALUES(?,?,?,?,?,?,?)", item.id, "http://127.0.0.1:11434", "test:1", strings.Repeat("a", 64), learned.ContentHash(text), 2, embed.Encode([]float32{1, 0})); err != nil {
			t.Fatal(err)
		}
		if item.source != "" {
			if _, err := db.Exec("INSERT INTO memory_imports VALUES(?,?,?,?,?,?)", item.source, "trace-import", "turn", item.id, "fingerprint", item.id); err != nil {
				t.Fatal(err)
			}
		}
	}
	modelDigest := strings.Repeat("a", 64)
	calls := 0
	factory := func(endpoint string) (modelProvider, error) {
		calls++
		if endpoint != "http://127.0.0.1:11434" {
			t.Error("endpoint changed")
		}
		return &fakeLocalModel{digest: func() string { return modelDigest }, embed: func(texts []string) [][]float32 {
			if strings.Contains(texts[0], "ghp_abcdefghijklmnopqrstuvwxyz") {
				t.Error("unredacted query")
			}
			return [][]float32{{1, 0}}
		}}, nil
	}
	run := func(args ...string) memoryContext {
		t.Helper()
		var output bytes.Buffer
		command := newRootWithModelProvider(factory)
		command.SetOut(&output)
		command.SetErr(&bytes.Buffer{})
		command.SetArgs(append([]string{"--db", path, "context", "--trace-db", sourceA, "--query", "automobile ghp_abcdefghijklmnopqrstuvwxyz", "--learned-model", "test:1"}, args...))
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		var out memoryContext
		if err := json.Unmarshal(output.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := run()
	if out.Status != "ready" || out.Retrieval != "learned_local_ollama" || len(out.Memories) != 3 || out.Learned == nil || out.Learned.DocumentsIncluded {
		t.Fatalf("wrong learned context: %+v", out)
	}
	for i, want := range []string{"manual", "mcp", "own-trace"} {
		if out.Memories[i].ID != want || out.Memories[i].Score == nil || *out.Memories[i].Score != 1 {
			t.Fatalf("wrong rank/provenance: %+v", out.Memories)
		}
	}
	out = run("--learned-include-documents")
	if len(out.Memories) != 4 || out.Memories[0].ID != "local-doc" || !out.Learned.DocumentsIncluded {
		t.Fatalf("explicit document corpus lost: %+v", out)
	}
	out = run("--learned-include-documents", "--learned-scope", "documents")
	if len(out.Memories) != 1 || out.Memories[0].ID != "local-doc" {
		t.Fatalf("scope crossed: %+v", out)
	}
	modelDigest = strings.Repeat("b", 64)
	out = run("--query", "car")
	if out.Status != "needs_embeddings" || len(out.Memories) != 0 || out.Learned.MissingVectors != 3 {
		t.Fatalf("keyword fallback or mixed models: %+v", out)
	}
	if calls != 4 {
		t.Fatal("unexpected inference count", calls)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM semantic_embeddings").Scan(&count); err != nil || count != 5 {
		t.Fatal("context mutated learned cache", err)
	}
	command := newRootWithModelProvider(func(string) (modelProvider, error) { t.Fatal("ordinary context contacted model"); return nil, nil })
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--db", path, "context", "--trace-db", sourceA, "--query", "car"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestRegressionLearnedContextMissingIndexAndInvalidInputNeverProbeOrCreateState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "m.db")
	for _, test := range []struct {
		args    []string
		missing bool
	}{
		{[]string{"--query", "car", "--learned-model", "test:1"}, true},
		{args: []string{"--query", "car", "--learned-endpoint", "http://127.0.0.1:11434"}},
		{args: []string{"--query", "car", "--learned-model", "test:1", "--semantic"}},
		{args: []string{"--query", "car", "--learned-model", "test:1", "--learned-endpoint", "https://example.com"}},
		{args: []string{"--learned-model", "test:1"}},
		{args: []string{"--query", "car", "--learned-model", "ghp_abcdefghijklmnopqrstuvwxyz"}},
	} {
		var output bytes.Buffer
		command := newRootWithModelProvider(func(string) (modelProvider, error) { t.Fatal("missing/invalid context probed model"); return nil, nil })
		command.SetOut(&output)
		command.SetErr(&bytes.Buffer{})
		command.SetArgs(append([]string{"--db", path, "context"}, test.args...))
		err := command.Execute()
		if test.missing {
			var out memoryContext
			if err != nil || json.Unmarshal(output.Bytes(), &out) != nil || out.Status != "missing_index" || out.Learned.ModelStatus != "not_probed" {
				t.Fatalf("missing context: %s %v", &output, err)
			}
		} else if err == nil || output.Len() != 0 {
			t.Fatalf("invalid context emitted data: %s %v", &output, err)
		}
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Fatal("context created state", err)
		}
	}
}
