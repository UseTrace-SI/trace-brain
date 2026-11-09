package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/UseTrace-SI/trace-brain/internal/store"
)

func TestWorkspaceRetrievalIsExplicitScopedBoundedAndReadOnly(t *testing.T) {
	paths := []string{}
	for _, name := range []string{"a", "b"} {
		path := filepath.Join(t.TempDir(), "memory.db")
		paths = append(paths, path)
		db, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Remember(context.Background(), db, "same-id", "project", "authentication retry "+name, "manual", 1); err != nil {
			t.Fatal(err)
		}
		if err := store.Remember(context.Background(), db, "private", "other", "authentication private scope", "manual", 2); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	cmd := newRoot()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"workspace", "query", "authentication()", "--databases", paths[0], "--databases", paths[1], "--databases", paths[0], "--databases", missing, "--scope", "project", "--limit", "2"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Memories []workspaceMemory `json:"memories"`
		Sources  []workspaceSource `json:"sources"`
		Partial  bool              `json:"partial"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Partial || len(report.Sources) != 3 || len(report.Memories) != 2 || report.Memories[0].Database == report.Memories[1].Database {
		t.Fatalf("wrong federation: %+v", report)
	}
	for _, m := range report.Memories {
		if m.Scope != "project" {
			t.Fatal("scope filter ignored")
		}
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("retrieval created a missing database")
	}
}
