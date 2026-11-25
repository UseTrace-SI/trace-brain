package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// testdata/schema-v0-memory.db was written by trace-brain at commit 3ec76fe
// (the last release without PRAGMA user_version), through its CLI: `setup`
// from a two-turn Trace history (with rule-extracted facts), four `remember`
// notes, one `update`, a `documents sync`, a `facts assert` with captured
// evidence and one cached learned vector. It is never modified in place.
const v0Fixture = "testdata/schema-v0-memory.db"

var fixtureTables = []string{"memories", "memory_revisions", "memory_imports", "embeddings", "semantic_embeddings",
	"document_sources", "document_chunks", "facts", "fact_evidence", "fact_evidence_usage", "fact_sources",
	"fact_extraction_state", "memory_source_times"}

func copyFixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(v0Fixture)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "memory.db")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// snapshotTables returns every row of the fixture tables keyed by column name.
func snapshotTables(t *testing.T, db *sql.DB) map[string][]map[string]any {
	t.Helper()
	out := map[string][]map[string]any{}
	for _, table := range fixtureTables {
		rows, err := db.Query("SELECT * FROM " + table + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		out[table] = []map[string]any{}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			row := map[string]any{}
			for i, column := range columns {
				if b, ok := values[i].([]byte); ok {
					values[i] = string(b)
				}
				row[column] = values[i]
			}
			out[table] = append(out[table], row)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestMigrationUpgradesPreVersioningDatabaseWithoutDataLoss(t *testing.T) {
	path := copyFixture(t)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 0 {
		t.Fatalf("fixture is not a pre-versioning database: %d %v", version, err)
	}
	before := snapshotTables(t, raw)
	raw.Close()
	if len(before["memories"]) != 8 || len(before["facts"]) != 2 || len(before["fact_evidence"]) != 1 || len(before["semantic_embeddings"]) != 1 {
		t.Fatalf("unexpected fixture contents: %d memories", len(before["memories"]))
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("schema version %d, want %d (%v)", version, SchemaVersion, err)
	}
	after := snapshotTables(t, db)
	for _, table := range fixtureTables {
		if len(after[table]) != len(before[table]) {
			t.Fatalf("%s: %d rows after migration, %d before", table, len(after[table]), len(before[table]))
		}
		for i, row := range before[table] {
			for column, value := range row {
				if !reflect.DeepEqual(after[table][i][column], value) {
					t.Fatalf("%s row %d column %s changed: %v -> %v", table, i, column, value, after[table][i][column])
				}
			}
		}
	}
	if _, err := db.Exec("INSERT INTO memory_fts(memory_fts) VALUES('integrity-check')"); err != nil {
		t.Fatalf("keyword index inconsistent after migration: %v", err)
	}
	// Triggers still maintain the index and revision cap after the upgrade.
	if _, err := db.Exec("UPDATE memories SET content='upgraded keyword' WHERE content LIKE 'the cat%'"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM memory_fts WHERE memory_fts MATCH 'upgraded'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("index trigger lost: %d %v", n, err)
	}
	if _, err := db.Exec("INSERT INTO memory_fts(memory_fts) VALUES('integrity-check')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	// Re-opening an up-to-date database is a no-op that keeps the version.
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("reopen changed version: %d %v", version, err)
	}
}

func TestMigrationRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", SchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("opened a database from a newer release: %v", err)
	}
}

func TestMigrationFreshDatabaseIsCurrent(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("fresh schema version %d (%v)", version, err)
	}
	// Up-to-date opens must not prune revisions: that cleanup belongs to the
	// one-time baseline step. Insert beyond the cap with the trigger disabled.
	if _, err := db.Exec(`INSERT INTO memories(id,scope,content,created_at) VALUES('m','p','c',1); DROP TRIGGER memory_revisions_cap;
WITH RECURSIVE seq(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM seq WHERE x<60) INSERT INTO memory_revisions SELECT 'm',x,'p','c',x FROM seq`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM memory_revisions").Scan(&n); err != nil || n != 60 {
		t.Fatalf("up-to-date open rewrote revisions: %d %v", n, err)
	}
}
