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

// runtimeCredential builds a 40-character mixed-class, high-entropy value at
// runtime, so the source tree never contains a credential-shaped literal that
// push protection would flag. The redactor recognizes this shape as a secret.
func runtimeCredential() string {
	const alphabet = "Ab3Cd5Ef7Gh9Jk1Lm2Np4Qr6"
	value := make([]byte, 40)
	for i := range value {
		value[i] = alphabet[i%len(alphabet)]
	}
	return string(value)
}

func TestRegressionHistoryReappliesRedactionOnRead(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	secret := runtimeCredential()
	// Write a revision directly to emulate a legacy or corrupted row whose
	// content escaped write-time redaction. Reads must re-apply the policy.
	if _, err := db.Exec("INSERT INTO memories(id,scope,content,source,created_at) VALUES('m','project','seed','manual',1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO memory_revisions(memory_id,revision,scope,content,recorded_at) VALUES('m',1,?,?,2)", "scope "+secret, "content "+secret); err != nil {
		t.Fatal(err)
	}
	revisions, err := History(ctx, db, "m")
	if err != nil || len(revisions) != 1 {
		t.Fatalf("history: %+v %v", revisions, err)
	}
	r := revisions[0]
	if strings.Contains(r.Content, secret) || strings.Contains(r.Scope, secret) || !strings.Contains(r.Content, "redacted:") || !strings.Contains(r.Scope, "redacted:") {
		t.Fatalf("history leaked unredacted revision: %+v", r)
	}
}

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

func TestRegressionMemoryRevisionRetentionIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	ctx := context.Background()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Remember(ctx, db, "m", "project", "initial", "mcp", 1); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 60; i++ {
		if err := Update(ctx, db, "m", "", fmt.Sprintf("update %d", i), int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM memory_revisions WHERE memory_id='m'").Scan(&count); err != nil || count != 50 {
		t.Fatalf("unbounded revisions: %d %v", count, err)
	}
	revisions, err := History(ctx, db, "m")
	if err != nil || len(revisions) != 50 || revisions[0].Revision != 60 || revisions[len(revisions)-1].Revision != 11 {
		t.Fatalf("wrong retained revisions: %d %+v %v", len(revisions), revisions, err)
	}
	// Simulate a database that accumulated revisions before the cap existed.
	if _, err := db.Exec("DROP TRIGGER memory_revisions_cap"); err != nil {
		t.Fatal(err)
	}
	for i := 61; i <= 70; i++ {
		if _, err := db.Exec("INSERT INTO memory_revisions(memory_id,revision,scope,content,recorded_at) VALUES('m',?,?,?,?)", i, "project", "old", int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow("SELECT count(*) FROM memory_revisions WHERE memory_id='m'").Scan(&count); err != nil || count != 50 {
		t.Fatalf("migration left unbounded revisions: %d %v", count, err)
	}
	revisions, err = History(ctx, db, "m")
	if err != nil || len(revisions) != 50 || revisions[0].Revision != 70 {
		t.Fatalf("migration kept wrong revisions: %+v %v", revisions, err)
	}
}

func TestRegressionUpdateRejectsOversizeAfterRedaction(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Remember(ctx, db, "m", "project", "original", "mcp", 1); err != nil {
		t.Fatal(err)
	}
	secret := "ghp_abcdefghijklmnopqrst"
	content := strings.Repeat("a", 65536-len(secret)-1) + " " + secret
	scope := strings.Repeat("s", 1024-len(secret)-1) + " " + secret
	if err := Update(ctx, db, "m", "", content, 2); err == nil {
		t.Fatal("accepted updated content that exceeds the budget after redaction")
	}
	if err := Update(ctx, db, "m", scope, "replacement", 3); err == nil {
		t.Fatal("accepted updated scope that exceeds the budget after redaction")
	}
	m, err := Get(ctx, db, "m")
	if err != nil || m.Content != "original" || m.Scope != "project" {
		t.Fatalf("rejected update mutated the memory: %+v %v", m, err)
	}
	versions, err := History(ctx, db, "m")
	if err != nil || len(versions) != 1 {
		t.Fatalf("rejected update created a revision: %+v %v", versions, err)
	}
}
