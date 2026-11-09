package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func documentInput(path, content string) DocumentInput {
	return DocumentInput{Path: path, Content: content, Fingerprint: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}
}
func TestDocumentRefreshIsAtomicRedactedAndSourceIsolated(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	docs := []DocumentInput{documentInput("guide.md", "originalkeyword sk-abcdefghijklmnopqrstuvwxyz1234567890")}
	sync := func(root string, docs []DocumentInput, prune bool) DocumentSync {
		t.Helper()
		result, err := SyncDocuments(ctx, db, root, "docs", docs, prune, 100)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if got := sync("/source", docs, false); got.Imported != 1 || got.Chunks != 1 || got.SourceFilesChanged {
		t.Fatalf("bad import: %+v", got)
	}
	var id, content string
	if err := db.QueryRow("SELECT id,content FROM memories WHERE source='document'").Scan(&id, &content); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "sk-abcdefghijklmnopqrstuvwxyz") || !strings.Contains(content, "redacted") {
		t.Fatal("document was not redacted")
	}
	if err := Update(ctx, db, id, "", "manual overwrite", 101); err == nil {
		t.Fatal("document memory was editable as a manual note")
	}
	if got := sync("/source", docs, false); got.Unchanged != 1 || got.Imported != 0 {
		t.Fatalf("refresh not idempotent: %+v", got)
	}
	if _, err := db.Exec("INSERT INTO semantic_embeddings VALUES(?, 'http://localhost', 'model', 'digest', 'content', 1, X'00')", id); err != nil {
		t.Fatal(err)
	}
	sync("/other", docs, false)
	changed := []DocumentInput{documentInput("guide.md", "newkeyword 中文 🧠")}
	if got := sync("/source", changed, false); got.Imported != 1 {
		t.Fatal(got)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM semantic_embeddings WHERE memory_id=?", id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stale learned vector: %d %v", count, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM memory_fts JOIN memories m ON m.rowid=memory_fts.rowid WHERE memory_fts MATCH 'originalkeyword' AND m.id=?", id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stale lexical result: %d %v", count, err)
	}
	if got := sync("/source", nil, false); got.Missing != 1 || got.Pruned != 0 {
		t.Fatal(got)
	}
	if got := sync("/source", nil, true); got.Pruned != 1 {
		t.Fatal(got)
	}
	if err := db.QueryRow("SELECT count(*) FROM memories").Scan(&count); err != nil || count != 1 {
		t.Fatalf("other source changed: %d %v", count, err)
	}
	if _, err := db.Exec("CREATE TRIGGER reject_document_vector BEFORE INSERT ON embeddings BEGIN SELECT RAISE(ABORT,'test failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncDocuments(ctx, db, "/other", "docs", changed, true, 200); err == nil {
		t.Fatal("failed import committed")
	}
	records, err := ListDocuments(ctx, db, "/other")
	if err != nil || len(records) != 1 || records[0].Fingerprint != docs[0].Fingerprint {
		t.Fatalf("partial refresh: %+v %v", records, err)
	}
}
func TestDocumentRefreshRepairsForgottenChunksAndPreservesUnicodeBoundaries(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	docs := []DocumentInput{documentInput("guide.txt", strings.Repeat("🧠", 4100))}
	first, err := SyncDocuments(ctx, db, "/source", "docs", docs, false, 1)
	if err != nil || first.Chunks != 2 {
		t.Fatalf("chunking: %+v %v", first, err)
	}
	var id string
	if err := db.QueryRow("SELECT memory_id FROM document_chunks LIMIT 1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := Forget(ctx, db, id); err != nil {
		t.Fatal(err)
	}
	next, err := SyncDocuments(ctx, db, "/source", "docs", docs, false, 2)
	if err != nil || next.Imported != 1 {
		t.Fatalf("did not repair removed chunk: %+v %v", next, err)
	}
}
