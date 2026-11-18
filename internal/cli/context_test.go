package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-brain/internal/store"
)

// Equal hashed scores and equal recency must still resolve in a deterministic
// ID order, matching semantic.Search's score/recency/ID tie-break.
func TestContextSemanticTiesBreakDeterministicallyByID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, id := range []string{"seed-c", "seed-a", "seed-b"} {
		if err := store.Remember(context.Background(), db, id, "project", "car car car", "manual", 42); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"seed-a", "seed-b", "seed-c"}
	for i := 0; i < 5; i++ {
		var out bytes.Buffer
		cmd := newRoot()
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--db", path, "context", "--semantic", "--query", "car", "--limit", "3"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		var got memoryContext
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Memories) != len(want) {
			t.Fatalf("expected %d memories, got %d: %+v", len(want), len(got.Memories), got)
		}
		for j, item := range got.Memories {
			if item.ID != want[j] {
				t.Fatalf("unstable tie order: got %v, want %v", idsOf(got.Memories), want)
			}
		}
	}
}

func idsOf(items []memoryItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func TestCompanionRefreshAndReadOnlyContext(t *testing.T) {
	dir := t.TempDir()
	path, source := filepath.Join(dir, "memory.db"), filepath.Join(dir, "trace.db")
	src, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.Exec(`CREATE TABLE sessions(id TEXT,agent TEXT,worktree TEXT,started_at INTEGER); INSERT INTO sessions VALUES('s','codex','repo',1);
CREATE TABLE turns(id TEXT,session_id TEXT,seq INTEGER,prompt TEXT,reasoning TEXT); INSERT INTO turns VALUES('t','s',1,'Authentication timeout','Bound the request');`); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) []byte {
		t.Helper()
		var out bytes.Buffer
		cmd := newRoot()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--db", path}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, &out)
		}
		return out.Bytes()
	}
	var got memoryContext
	if err := json.Unmarshal(run("context", "--query", "authentication (timeout):"), &got); err != nil || got.Status != "missing_index" {
		t.Fatalf("%+v, %v", got, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("read-only context created a database")
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO memories(id,scope,content,source,created_at) VALUES('manual','project','manual authentication note','manual',1)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for i := 0; i < 2; i++ {
		run("refresh", "--trace-db", source)
	}
	if err := json.Unmarshal(run("context", "--query", "authentication (timeout):", "--limit", "50"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Protocol != 1 || got.Status != "ready" || len(got.Memories) != 2 {
		t.Fatalf("%+v", got)
	}
	foundTurn, foundManual := false, false
	for _, m := range got.Memories {
		foundTurn = foundTurn || m.TurnID == "t"
		foundManual = foundManual || m.Source == "manual"
	}
	if !foundTurn || !foundManual {
		t.Fatal("refresh lost a source link or manual memory")
	}
	otherSource := filepath.Join(dir, "other-trace.db")
	other, err := sql.Open("sqlite", otherSource)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(`CREATE TABLE sessions(id TEXT,agent TEXT,worktree TEXT,started_at INTEGER); INSERT INTO sessions VALUES('other-s','codex','other-repo',1);
CREATE TABLE turns(id TEXT,session_id TEXT,seq INTEGER,prompt TEXT,reasoning TEXT); INSERT INTO turns VALUES('other-t','other-s',1,'authentication from another project','');`); err != nil {
		t.Fatal(err)
	}
	other.Close()
	run("refresh", "--trace-db", otherSource)
	if err := json.Unmarshal(run("context", "--trace-db", source, "--query", "authentication"), &got); err != nil || len(got.Memories) != 2 {
		t.Fatalf("cross-project memory or missing local note: %+v %v", got, err)
	}
	for _, memory := range got.Memories {
		if memory.TurnID == "other-t" {
			t.Fatal("context crossed Trace source boundaries")
		}
	}
	if _, err := src.Exec("UPDATE turns SET prompt='Authorization changed'"); err != nil {
		t.Fatal(err)
	}
	run("refresh", "--trace-db", source)
	if err := json.Unmarshal(run("context", "--query", "Authorization"), &got); err != nil || len(got.Memories) != 1 || !strings.Contains(got.Memories[0].Content, "changed") {
		t.Fatalf("stale import: %+v %v", got, err)
	}
}
