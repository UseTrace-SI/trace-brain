package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/UseTrace-SI/trace-brain/internal/embed"
	"github.com/UseTrace-SI/trace-brain/internal/redact"
)

type Memory struct {
	ID        string `json:"id"`
	Scope     string `json:"scope"`
	Content   string `json:"content"`
	Source    string `json:"source"`
	SessionID string `json:"session_id,omitempty"`
	TurnID    string `json:"turn_id,omitempty"`
	CreatedAt int64  `json:"created_at"`
}
type Revision struct {
	Revision         int    `json:"revision"`
	Scope            string `json:"scope"`
	Content          string `json:"content"`
	RecordedAt       int64  `json:"recorded_at"`
	ContentTruncated bool   `json:"content_truncated,omitempty"`
}

func Get(ctx context.Context, db *sql.DB, id string) (Memory, error) {
	var m Memory
	err := db.QueryRowContext(ctx, `SELECT id,scope,substr(content,1,65536),COALESCE(source,''),COALESCE(session_id,''),COALESCE(turn_id,''),created_at FROM memories WHERE id=?`, id).Scan(&m.ID, &m.Scope, &m.Content, &m.Source, &m.SessionID, &m.TurnID, &m.CreatedAt)
	m.Content = (redact.Redactor{}).Text(m.Content)
	m.Scope = (redact.Redactor{}).Text(m.Scope)
	return m, err
}

// Update revises user-authored memories atomically with their search vector.
// Derived memories remain owned by their source and cannot be silently edited.
func Update(ctx context.Context, db *sql.DB, id, scope, content string, at int64) error {
	if strings.TrimSpace(content) == "" || len(content) > 65536 || len(scope) > 1024 {
		return fmt.Errorf("memory must contain 1–65536 bytes and scope at most 1024 bytes")
	}
	content = (redact.Redactor{}).Text(content)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldContent, oldScope, source string
	var created int64
	if err := tx.QueryRowContext(ctx, "SELECT content,scope,COALESCE(source,''),created_at FROM memories WHERE id=?", id).Scan(&oldContent, &oldScope, &source, &created); err != nil {
		return err
	}
	if source != "manual" && source != "mcp" {
		return fmt.Errorf("derived memory is source-owned; save a manual memory instead")
	}
	if scope == "" {
		scope = oldScope
	}
	scope = (redact.Redactor{}).Text(scope)
	if _, err := tx.ExecContext(ctx, "INSERT INTO memory_revisions(memory_id,revision,scope,content,recorded_at) VALUES(?,1,?,?,?) ON CONFLICT(memory_id,revision) DO NOTHING", id, (redact.Redactor{}).Text(oldScope), (redact.Redactor{}).Text(oldContent), created); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO memory_revisions(memory_id,revision,scope,content,recorded_at) SELECT ?,coalesce(max(revision),0)+1,?,?,? FROM memory_revisions WHERE memory_id=?", id, scope, content, at, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE memories SET scope=?,content=? WHERE id=?", scope, content, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM semantic_embeddings WHERE memory_id=?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO embeddings(memory_id,dim,vector) VALUES(?,?,?) ON CONFLICT(memory_id) DO UPDATE SET dim=excluded.dim,vector=excluded.vector", id, embed.Dim, embed.Encode(embed.Embed(content))); err != nil {
		return err
	}
	return tx.Commit()
}

// Forget deletes the content and all derivative local copies, including prior
// revisions, import fingerprints, embeddings and the FTS entry. A later source
// refresh can reimport a derived memory if its source still exists.
func Forget(ctx context.Context, db *sql.DB, id string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM memories WHERE id=?", id).Scan(&exists); err != nil {
		return err
	}
	for _, table := range []string{"memory_revisions", "memory_imports", "embeddings", "semantic_embeddings"} {
		column := "memory_id"
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE "+column+"=?", id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM memories WHERE id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func History(ctx context.Context, db *sql.DB, id string) ([]Revision, error) {
	rows, err := db.QueryContext(ctx, "SELECT revision,scope,substr(content,1,65536),recorded_at FROM memory_revisions WHERE memory_id=? ORDER BY revision DESC LIMIT 50", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Revision{}
	for rows.Next() {
		var r Revision
		if err := rows.Scan(&r.Revision, &r.Scope, &r.Content, &r.RecordedAt); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
