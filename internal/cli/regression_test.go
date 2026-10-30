package cli

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/store"
)

func TestRegressionImportAndExtraction(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "trace.db")
	dbPath = filepath.Join(dir, "memory.db")
	src, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.Exec(`CREATE TABLE sessions(id TEXT,agent TEXT,worktree TEXT,started_at INTEGER); INSERT INTO sessions VALUES('s','codex','repo',1);
CREATE TABLE turns(id TEXT,session_id TEXT,seq INTEGER,prompt TEXT,reasoning TEXT); INSERT INTO turns VALUES('t','s',1,'oldword','context');`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := importTrace(context.Background(), io.Discard, source, "", false); err != nil {
			t.Fatal(err)
		}
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM memories").Scan(&n); err != nil || n != 2 {
		t.Fatalf("duplicates: %d %v", n, err)
	}
	if _, err := src.Exec("UPDATE turns SET prompt='newword'"); err != nil {
		t.Fatal(err)
	}
	if err := importTrace(context.Background(), io.Discard, source, "", false); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM memory_fts WHERE memory_fts MATCH 'oldword'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("stale FTS: %d %v", n, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM memory_fts WHERE memory_fts MATCH 'newword'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("missing FTS: %d %v", n, err)
	}
	calls := filepath.Join(dir, "calls")
	agent := "cat >/dev/null; echo call >> '" + calls + "'; echo extracted"
	for i := 0; i < 2; i++ {
		if err := importTrace(context.Background(), io.Discard, source, agent, true); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(calls)
	if err != nil || string(b) != "call\n" {
		t.Fatalf("unchanged extraction reran: %q %v", b, err)
	}
	// A shell-launched agent can spawn children that inherit stdout. Stopping
	// extraction must cancel those children, not wait for their inherited pipe.
	marker := filepath.Join(dir, "agent-started")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- importTrace(ctx, io.Discard, source, "echo started > '"+marker+"'; sleep 30", true) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("extraction agent did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled extraction reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent child prevented cancellation")
	}
	if _, err := src.Exec("DROP TABLE turns"); err != nil {
		t.Fatal(err)
	}
	if err := importTrace(context.Background(), io.Discard, source, "", false); err == nil {
		t.Fatal("import failure swallowed")
	}
}

func TestRegressionQueryAndEscapedVisualization(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.db")
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
	run("remember", "--scope", "<svg onload=alert(1)>", "<script>alert(1)</script> searchable")
	if out := run("query", "searchable"); !strings.Contains(out, "searchable") {
		t.Fatalf("query failed: %s", out)
	}
	run("viz")
	b, err := os.ReadFile(filepath.Join(dir, "viz.html"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "<script>") || strings.Contains(string(b), "<svg") || !strings.Contains(string(b), "&lt;script&gt;") {
		t.Fatalf("unsafe visualization: %s", b)
	}
}

func TestRegressionWatcherIdentityAndShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	// A PID belonging to this test process must never be signaled.
	if err := os.WriteFile(path+".watch.pid", []byte("1"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := stopWatcher(path); err == nil {
		t.Fatal("accepted unauthenticated PID file")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runWatcher(ctx, path, time.Hour, func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() })
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("watcher failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not start")
	}
	if err := os.WriteFile(path+".watch.stop", []byte(strings.Repeat("0", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("unauthenticated stop request ended watcher: %v", err)
	case <-time.After(60 * time.Millisecond):
	}
	if err := runWatcher(ctx, path, time.Hour, func(context.Context) error { return nil }); err == nil {
		t.Fatal("duplicate watcher admitted")
	}
	if err := stopWatcher(path); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not stop")
	}
	if _, err := os.Stat(path + ".watch.pid"); !os.IsNotExist(err) {
		t.Fatalf("stale PID file: %v", err)
	}
}
