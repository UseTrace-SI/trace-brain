package cli

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/UseTrace-SI/trace-brain/internal/store"
)

// Trace appends assistant output to a turn without a size limit. One oversized
// or malformed turn used to make every setup/refresh fail with nothing
// imported; it must instead be bounded while the rest of the history imports.
func TestRegressionImportBoundsOversizedAndMalformedTurnsWithoutBlockingHistory(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "trace.db")
	dbPath = filepath.Join(dir, "memory.db")
	src, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.Exec(`CREATE TABLE sessions(id TEXT,agent TEXT,worktree TEXT,started_at INTEGER);
		CREATE TABLE turns(id TEXT,session_id TEXT,seq INTEGER,prompt TEXT,reasoning TEXT);
		INSERT INTO sessions VALUES('s','codex','repo',1);
		INSERT INTO turns VALUES('small','s',1,'small prompt','ok');`); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Exec(`INSERT INTO turns VALUES('large','s',2,'large prompt',?),('malformed','s',3,?,'')`,
		strings.Repeat("Assistant response: still working. ", 3000)+"FINAL-OUTCOME", "bad \xff\xfe bytes"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := importTrace(context.Background(), &out, source, "", false); err != nil {
		t.Fatalf("one history entry blocked the import: %v", err)
	}
	if got := out.String(); got != "imported 1 sessions\nimported 3 turns\n" {
		t.Fatalf("unexpected import report: %q", got)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	contents := map[string]string{}
	rows, err := db.Query("SELECT COALESCE(turn_id,''),content FROM memories")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var turn, content string
		if err := rows.Scan(&turn, &content); err != nil {
			t.Fatal(err)
		}
		if len(content) > 65536 || !utf8.ValidString(content) {
			t.Fatalf("turn %q stored %d bytes, valid UTF-8=%v", turn, len(content), utf8.ValidString(content))
		}
		contents[turn] = content
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(contents) != 4 || !strings.Contains(contents["small"], "small prompt") {
		t.Fatalf("history was not imported: %d memories", len(contents))
	}
	large := contents["large"]
	if !strings.Contains(large, "large prompt") || !strings.HasSuffix(large, "FINAL-OUTCOME") || !strings.Contains(large, "bytes omitted") {
		t.Fatalf("oversized turn lost its prompt, outcome or omission notice: %q...%q", large[:64], large[len(large)-64:])
	}
	if !strings.Contains(contents["malformed"], "bad � bytes") {
		t.Fatalf("malformed turn was not normalized: %q", contents["malformed"])
	}
	out.Reset()
	if err := importTrace(context.Background(), &out, source, "", false); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "imported 0 sessions\nimported 0 turns\n" {
		t.Fatalf("bounded import is not idempotent: %q", got)
	}
}
