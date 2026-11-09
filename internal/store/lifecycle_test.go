package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/UseTrace-SI/trace-brain/internal/embed"
)

func TestMemoryLifecyclePreservesRevisionsAndForgetsAllCopies(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Remember(ctx, db, "m", "project", "original lexicalterm", "manual", 10); err != nil {
		t.Fatal(err)
	}
	secret := "sk-abcdefghijklmnopqrstuvwxyz1234567890"
	if err := Update(ctx, db, "m", "", "updated keyword "+secret, 20); err != nil {
		t.Fatal(err)
	}
	m, err := Get(ctx, db, "m")
	if err != nil || m.Scope != "project" || m.CreatedAt != 10 || strings.Contains(m.Content, secret) {
		t.Fatalf("invalid update: %+v %v", m, err)
	}
	history, err := History(ctx, db, "m")
	if err != nil || len(history) != 2 || history[0].RecordedAt != 20 || history[1].Content != "original lexicalterm" || strings.Contains(history[0].Content, secret) {
		t.Fatalf("lost or unsafe history: %+v %v", history, err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM memory_fts WHERE memory_fts MATCH 'lexicalterm'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("obsolete FTS: %d %v", count, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM memory_fts WHERE memory_fts MATCH 'keyword'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("missing FTS: %d %v", count, err)
	}
	var vector []byte
	if err := db.QueryRow("SELECT vector FROM embeddings WHERE memory_id='m'").Scan(&vector); err != nil {
		t.Fatal(err)
	}
	if embed.Cosine(embed.Embed(m.Content), embed.Decode(vector, embed.Dim)) < 0.999 {
		t.Fatal("stale vector after update")
	}
	if _, err := db.Exec("INSERT INTO memory_imports VALUES('source','trace-import','turn','t','f','m')"); err != nil {
		t.Fatal(err)
	}
	if err := Forget(ctx, db, "m"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"memories", "embeddings", "memory_revisions", "memory_imports", "memory_fts", "semantic_embeddings"} {
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("forgotten content remains in %s: %d %v", table, count, err)
		}
	}
	if _, err := Get(ctx, db, "m"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("forgotten memory readable: %v", err)
	}
}

func TestReopenedConnectionsKeepForeignKeysForAllMemoryDerivatives(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Remember(context.Background(), db, "m", "project", "memory", "manual", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO semantic_embeddings VALUES('m','local','model','digest','hash',1,x'0000803f')"); err != nil {
		t.Fatal(err)
	}
	db.SetMaxIdleConns(0)
	if _, err := db.Exec("DELETE FROM memories WHERE id='m'"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"embeddings", "memory_revisions", "semantic_embeddings"} {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("reopened connection left %s: %d %v", table, n, err)
		}
	}
}

func TestMemoryConcurrentUpdatesAreVersionedAndDerivedMemoriesStaySourceOwned(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Remember(ctx, db, "m", "project", "initial", "mcp", 1); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- Update(ctx, db, "m", "", fmt.Sprintf("update %d", i), int64(i+2))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	items, err := History(ctx, db, "m")
	if err != nil || len(items) != 21 || items[0].Revision != 21 {
		t.Fatalf("lost concurrent version: %+v %v", items, err)
	}
	if _, err := db.Exec("UPDATE memories SET source='trace-import' WHERE id='m'"); err != nil {
		t.Fatal(err)
	}
	if err := Update(ctx, db, "m", "", "do not overwrite", 100); err == nil {
		t.Fatal("edited a source-owned memory")
	}
	if err := Update(ctx, db, "m", "", "", 100); err == nil {
		t.Fatal("accepted empty memory")
	}
}
