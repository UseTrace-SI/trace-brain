package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON"); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS memories (id TEXT PRIMARY KEY, scope TEXT NOT NULL, content TEXT NOT NULL,
  source TEXT, session_id TEXT, turn_id TEXT, created_at INTEGER NOT NULL);
CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(content, content='memories', content_rowid='rowid');
CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
  INSERT INTO memory_fts(rowid,content) VALUES(new.rowid,new.content);
END;
CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
  INSERT INTO memory_fts(memory_fts,rowid,content) VALUES('delete',old.rowid,old.content);
END;
CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
 INSERT INTO memory_fts(memory_fts,rowid,content) VALUES('delete',old.rowid,old.content);
 INSERT INTO memory_fts(rowid,content) VALUES(new.rowid,new.content);
END;
CREATE TABLE IF NOT EXISTS memory_imports (
 source_db TEXT NOT NULL, source TEXT NOT NULL, entity_kind TEXT NOT NULL, entity_id TEXT NOT NULL,
 fingerprint TEXT NOT NULL, memory_id TEXT NOT NULL,
 PRIMARY KEY(source_db,source,entity_kind,entity_id));
CREATE INDEX IF NOT EXISTS memories_scope ON memories(scope, created_at DESC);
CREATE INDEX IF NOT EXISTS memory_imports_memory ON memory_imports(memory_id,source_db);
CREATE TABLE IF NOT EXISTS embeddings (memory_id TEXT PRIMARY KEY REFERENCES memories(id) ON DELETE CASCADE,
  dim INTEGER NOT NULL, vector BLOB NOT NULL);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("migrating: %w", err)
	}
	return nil
}
