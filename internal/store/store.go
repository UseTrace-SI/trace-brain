package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

func Open(path string) (*sql.DB, error) {
	// Imports read fingerprints before writing. Reserve the writer at BEGIN
	// so another process cannot invalidate that read snapshot before the write.
	dsn := path + "?_txlock=immediate"
	if path != ":memory:" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		u := url.URL{Scheme: "file", Path: absolute, RawQuery: "_txlock=immediate"}
		dsn = u.String()
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// Concurrent first opens can collide while enabling WAL, before SQLite's
	// busy handler can wait. Retry only this idempotent initialization step.
	var initializeErr error
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, initializeErr = db.Exec("PRAGMA busy_timeout=30000; PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON")
		var sqliteError interface{ Code() int }
		if initializeErr == nil || !errors.As(initializeErr, &sqliteError) || sqliteError.Code()&255 != 5 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if initializeErr != nil {
		db.Close()
		return nil, fmt.Errorf("initializing database: %w", initializeErr)
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
