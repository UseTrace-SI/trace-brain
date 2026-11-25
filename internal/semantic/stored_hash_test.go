package semantic

import (
	"context"
	"strings"
	"testing"
)

// One malformed row used to fail the whole learned query. It is now skipped
// and counted, and the remaining ranking is returned.
func TestRegressionSemanticSkipsMalformedProvenanceWithWarning(t *testing.T) {
	db := searchDB(t)
	searchSeed(t, db, "good", "project", "a car", 2, []float32{1, 0})
	searchSeed(t, db, "long-turn", "project", "a car", 3, []float32{1, 0})
	searchSeed(t, db, "secret-session", "project", "a car", 4, []float32{1, 0})
	if _, err := db.Exec("UPDATE memories SET turn_id=? WHERE id='long-turn'; UPDATE memories SET session_id=? WHERE id='secret-session'",
		strings.Repeat("t", 300), "ghp_abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatal(err)
	}
	out, err := Search(context.Background(), db, &searchModel{}, searchRequest())
	if err != nil {
		t.Fatalf("malformed row failed the query: %v", err)
	}
	if len(out.Memories) != 1 || out.Memories[0].ID != "good" || out.SkippedMalformed != 2 || len(out.Warnings) != 1 || out.IndexedMemories != 3 {
		t.Fatalf("malformed rows not skipped and counted: %+v", out)
	}
	// Rows below the returned window are never validated or redacted.
	in := searchRequest()
	in.Limit = 1
	out, err = Search(context.Background(), db, &searchModel{}, in)
	if err != nil || len(out.Memories) != 1 || out.SkippedMalformed != 2 {
		t.Fatalf("limited query: %+v %v", out, err)
	}
	if _, err := db.Exec("UPDATE memories SET created_at=1 WHERE id IN ('long-turn','secret-session')"); err != nil {
		t.Fatal(err)
	}
	out, err = Search(context.Background(), db, &searchModel{}, in)
	if err != nil || len(out.Memories) != 1 || out.Memories[0].ID != "good" || out.SkippedMalformed != 0 || out.Warnings != nil {
		t.Fatalf("lower-ranked malformed rows were inspected: %+v %v", out, err)
	}
}

// Stored fingerprints are compared in SQL, but returned rows are still
// verified against their content, so a stale stored value cannot surface a
// vector for different text.
func TestRegressionSemanticVerifiesStoredFingerprintOfReturnedRows(t *testing.T) {
	db := searchDB(t)
	searchSeed(t, db, "fresh", "project", "a car", 1, []float32{1, 0})
	searchSeed(t, db, "tampered", "project", "a car", 2, []float32{1, 0})
	searchSeed(t, db, "mismatch", "project", "a car", 3, []float32{1, 0})
	var stored string
	if err := db.QueryRow("SELECT content_hash FROM memories WHERE id='fresh'").Scan(&stored); err != nil || stored != ContentHash("a car") {
		t.Fatalf("writer did not store the fingerprint: %q %v", stored, err)
	}
	// An external writer that keeps the old fingerprint while changing
	// content (with the reset trigger removed) and a fingerprint that
	// disagrees with the cached vector.
	if _, err := db.Exec(`DROP TRIGGER memories_content_hash_reset;
		UPDATE memories SET content='a truck' WHERE id='tampered';
		UPDATE memories SET content_hash='0000' WHERE id='mismatch'`); err != nil {
		t.Fatal(err)
	}
	out, err := Search(context.Background(), db, &searchModel{}, searchRequest())
	if err != nil || len(out.Memories) != 1 || out.Memories[0].ID != "fresh" || out.StaleVectors != 2 || out.IndexedMemories != 1 {
		t.Fatalf("stored fingerprint trusted without verification: %+v %v", out, err)
	}
}

// External content changes clear the stored fingerprint, and rows without
// one are verified from their content exactly as before.
func TestRegressionSemanticContentChangeClearsStoredFingerprint(t *testing.T) {
	db := searchDB(t)
	searchSeed(t, db, "m", "project", "a car", 1, []float32{1, 0})
	if _, err := db.Exec("UPDATE memories SET content='source changed' WHERE id='m'"); err != nil {
		t.Fatal(err)
	}
	var stored *string
	if err := db.QueryRow("SELECT content_hash FROM memories WHERE id='m'").Scan(&stored); err != nil || stored != nil {
		t.Fatalf("external content change kept fingerprint: %v %v", stored, err)
	}
	out, err := Search(context.Background(), db, &searchModel{}, searchRequest())
	if err != nil || out.StaleVectors != 1 || len(out.Memories) != 0 {
		t.Fatalf("changed content matched its old vector: %+v %v", out, err)
	}
	// A vector cached for the new text matches through the content check.
	if _, err := db.Exec("UPDATE semantic_embeddings SET content_hash=? WHERE memory_id='m'", ContentHash("source changed")); err != nil {
		t.Fatal(err)
	}
	out, err = Search(context.Background(), db, &searchModel{}, searchRequest())
	if err != nil || len(out.Memories) != 1 {
		t.Fatalf("unfingerprinted row not verified from content: %+v %v", out, err)
	}
}

// Learned reads never migrate, so a database opened by an older release has
// no fingerprint column; every row is then verified from its content.
func TestRegressionSemanticReadsDatabasesWithoutFingerprintColumn(t *testing.T) {
	db := searchDB(t)
	searchSeed(t, db, "m", "project", "a car", 1, []float32{1, 0})
	searchSeed(t, db, "stale", "project", "a car", 2, []float32{1, 0})
	if _, err := db.Exec(`DROP TRIGGER memories_content_hash_reset; ALTER TABLE memories DROP COLUMN content_hash;
		UPDATE memories SET content='changed' WHERE id='stale'; PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	out, err := Search(context.Background(), db, &searchModel{}, searchRequest())
	if err != nil || len(out.Memories) != 1 || out.Memories[0].ID != "m" || out.StaleVectors != 1 {
		t.Fatalf("pre-fingerprint database: %+v %v", out, err)
	}
}
