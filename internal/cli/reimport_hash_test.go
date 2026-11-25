package cli

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/UseTrace-SI/trace-brain/internal/redact"
	"github.com/UseTrace-SI/trace-brain/internal/store"
)

// Re-importing turns whose content differs only in marker digests (another
// marker key wrote them) must leave every fingerprint set, although the
// reset trigger clears it because the digest-free fingerprint is unchanged.
func TestReimportAfterMarkerOnlyChangeKeepsFingerprints(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "trace.db")
	dbPath = filepath.Join(dir, "memory.db")
	src, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.Exec(`CREATE TABLE sessions(id TEXT,agent TEXT,worktree TEXT,started_at INTEGER); INSERT INTO sessions VALUES('s','codex','repo',1);
CREATE TABLE turns(id TEXT,session_id TEXT,seq INTEGER,prompt TEXT,reasoning TEXT);`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if _, err := src.Exec("INSERT INTO turns VALUES(?,'s',?,?,'ok')", fmt.Sprintf("t%d", i), i, fmt.Sprintf("deploy %d with API_KEY=hunter2hunter2-%d", i, i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := importTrace(context.Background(), io.Discard, source, "", false); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Rewrite every stored marker digest as another key would have, keeping
	// the (digest-free) fingerprint, and invalidate the import fingerprints.
	rows, err := db.Query("SELECT id, content FROM memories")
	if err != nil {
		t.Fatal(err)
	}
	other := map[string]string{}
	digest := regexp.MustCompile(`(⟨redacted:[a-z-]+:)[0-9a-f]{8}⟩`)
	for rows.Next() {
		var id, content string
		if err := rows.Scan(&id, &content); err != nil {
			t.Fatal(err)
		}
		other[id] = digest.ReplaceAllString(content, "${1}00000000⟩")
	}
	rows.Close()
	changed := 0
	for id, content := range other {
		if _, err := db.Exec("UPDATE memories SET content=?, content_hash='seed' WHERE id=?", content, id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("UPDATE memories SET content_hash=? WHERE id=?", redact.Fingerprint(content), id); err != nil {
			t.Fatal(err)
		}
		changed++
	}
	if _, err := db.Exec("UPDATE memory_imports SET fingerprint='stale'"); err != nil {
		t.Fatal(err)
	}
	if err := importTrace(context.Background(), io.Discard, source, "", false); err != nil {
		t.Fatal(err)
	}
	var marked, missing int
	if err := db.QueryRow("SELECT count(*) FROM memories WHERE content LIKE '%⟨redacted:%' AND content NOT LIKE '%:00000000⟩%'").Scan(&marked); err != nil || marked != 30 {
		t.Fatalf("turns were not re-imported with current markers: %d %v", marked, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM memories WHERE content_hash IS NULL").Scan(&missing); err != nil || missing != 0 {
		t.Fatalf("%d of %d re-imported memories lost their fingerprint (%v)", missing, changed, err)
	}
}
