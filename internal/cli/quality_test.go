package cli

import (
	"context"
	"database/sql"
	"github.com/UseTrace-SI/trace-brain/internal/store"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractionUsesChangedEvidenceAndRedactsOutput(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "trace.db")
	dbPath = filepath.Join(dir, "memory.db")
	src, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.Exec(`CREATE TABLE sessions(id TEXT,agent TEXT,worktree TEXT,started_at INTEGER);INSERT INTO sessions VALUES('s','codex','repo',1);CREATE TABLE turns(id TEXT,session_id TEXT,seq INTEGER,prompt TEXT,reasoning TEXT);INSERT INTO turns VALUES('t','s',1,'fix authentication timeout','bounded request deadline');`); err != nil {
		t.Fatal(err)
	}
	captured := filepath.Join(dir, "evidence")
	agent := "cat > '" + captured + "'; printf 'decision sk-abcdefghijklmnopqrstuvwxyz1234567890'"
	if err := importTrace(context.Background(), io.Discard, source, agent, true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(captured)
	if err != nil || !strings.Contains(string(b), "authentication timeout") || !strings.Contains(string(b), "bounded request deadline") || !strings.Contains(string(b), `"id":"t"`) {
		t.Fatalf("missing extraction evidence: %s %v", b, err)
	}
	if _, err := src.Exec("UPDATE turns SET reasoning='new tested outcome'"); err != nil {
		t.Fatal(err)
	}
	if err := importTrace(context.Background(), io.Discard, source, agent, true); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(captured)
	if err != nil || !strings.Contains(string(b), "new tested outcome") {
		t.Fatal("session changes did not invalidate extraction")
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var content string
	if err := db.QueryRow("SELECT content FROM memories WHERE source='agent-extract'").Scan(&content); err != nil || strings.Contains(content, "sk-abcdefghijklmnopqrstuvwxyz") || !strings.Contains(content, "redacted") {
		t.Fatalf("unsafe extracted memory: %s %v", content, err)
	}
}
func TestRefreshRemovesDeletedDerivedMemoriesAndKeepsManual(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "trace.db")
	dbPath = filepath.Join(dir, "memory.db")
	src, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.Exec(`CREATE TABLE sessions(id TEXT,agent TEXT,worktree TEXT,started_at INTEGER);INSERT INTO sessions VALUES('s','codex','repo',1);CREATE TABLE turns(id TEXT,session_id TEXT,seq INTEGER,prompt TEXT,reasoning TEXT);INSERT INTO turns VALUES('t','s',1,'obsoleteword','old');`); err != nil {
		t.Fatal(err)
	}
	if err := importTrace(context.Background(), io.Discard, source, "", false); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := store.Remember(context.Background(), db, "manual", "project", "retained manual", "manual", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Exec("DELETE FROM turns"); err != nil {
		t.Fatal(err)
	}
	if err := importTrace(context.Background(), io.Discard, source, "", false); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM memory_fts WHERE memory_fts MATCH 'obsoleteword'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("stale derived memory: %d %v", n, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM memories WHERE id='manual'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("manual memory removed: %d %v", n, err)
	}
}
