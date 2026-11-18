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
	"time"
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
// The evidence fingerprint is read before the agent runs. If a concurrent
// writer changes it while the agent is working, the post-agent write must
// re-check inside its transaction and skip the now-stale extraction.
func TestRegressionConcurrentExtractRechecksFingerprintInWriteTx(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "trace.db")
	dbPath = filepath.Join(dir, "memory.db")
	src, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.Exec(`CREATE TABLE sessions(id TEXT,agent TEXT,worktree TEXT,started_at INTEGER);INSERT INTO sessions VALUES('s','codex','repo',1);CREATE TABLE turns(id TEXT,session_id TEXT,seq INTEGER,prompt TEXT,reasoning TEXT);INSERT INTO turns VALUES('t','s',1,'fix timeout','bounded deadline');`); err != nil {
		t.Fatal(err)
	}
	source, err = filepath.Abs(source)
	if err != nil {
		t.Fatal(err)
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memories(id,scope,content,source,created_at) VALUES('seed','sessions','seed content','agent-extract',1);
		INSERT INTO memory_imports VALUES(?,?,?,?,?,?)`, source, "agent-extract", "sessions", "s", "placeholder", "seed"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	agent := "cat >/dev/null; touch '" + started + "'; while [ ! -f '" + release + "' ]; do sleep 0.01; done; printf 'AGENT OUTPUT UNIQUE'"
	done := make(chan error, 1)
	go func() { done <- importTrace(context.Background(), io.Discard, source, agent, true) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("extraction agent did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// A concurrent writer invalidates the evidence while the agent is running.
	db, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE memory_imports SET fingerprint='concurrent-change' WHERE source_db=? AND source='agent-extract'", source); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("extraction did not finish")
	}
	db, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var content string
	if err := db.QueryRow("SELECT COALESCE(fingerprint,'') FROM memory_imports WHERE source_db=? AND source='agent-extract'", source).Scan(&content); err != nil || content != "concurrent-change" {
		t.Fatalf("stale extraction overwrote fingerprint: %q %v", content, err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM memories WHERE content LIKE '%AGENT OUTPUT UNIQUE%'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("stale extraction was persisted: %d %v", n, err)
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
