package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode"

	"github.com/UseTrace-SI/trace-brain/internal/embed"
	"github.com/UseTrace-SI/trace-brain/internal/redact"
)

// LiteralQuery treats task text as words rather than FTS operators.
func LiteralQuery(text string) (string, error) {
	if len(text) > 4096 {
		return "", fmt.Errorf("query exceeds 4096 bytes")
	}
	words := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' })
	if len(words) > 16 {
		words = words[:16]
	}
	for i := range words {
		words[i] = `"` + words[i] + `"`
	}
	if len(words) == 0 {
		return `""`, nil
	}
	return strings.Join(words, " OR "), nil
}

func Remember(ctx context.Context, db *sql.DB, id, scope, content, source string, at int64) error {
	if strings.TrimSpace(content) == "" || len(content) > 65536 || len(scope) > 1024 {
		return fmt.Errorf("memory must contain 1–65536 bytes and scope at most 1024 bytes")
	}
	content = (redact.Redactor{}).Text(content)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO memories(id,scope,content,source,created_at) VALUES(?,?,?,?,?)", id, scope, content, source, at); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO embeddings(memory_id,dim,vector) VALUES(?,?,?)", id, embed.Dim, embed.Encode(embed.Embed(content))); err != nil {
		return err
	}
	return tx.Commit()
}
