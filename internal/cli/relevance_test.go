package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/UseTrace-SI/trace-brain/internal/store"
	"path/filepath"
	"testing"
)

func TestContextRanksTaskEvidenceAheadOfNewerWeakMatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		id, content string
		at          int64
	}{{"specific", "authentication timeout retry deadline", 1}, {"recent", "authentication general notes", 2}, {"unrelated", "stylesheet colors", 3}} {
		if err := store.Remember(context.Background(), db, m.id, "project", m.content, "manual", m.at); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	var out bytes.Buffer
	cmd := newRoot()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--db", path, "context", "--query", "authentication timeout", "--limit", "1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var result memoryContext
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Memories) != 1 || result.Memories[0].ID != "specific" {
		t.Fatalf("weak recent match outranked task evidence: %s", out.String())
	}
}
