package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/UseTrace-SI/trace-brain/internal/embed"
	"github.com/UseTrace-SI/trace-brain/internal/redact"
)

const (
	maxMemoryContentBytes = 65536
	maxMemoryScopeBytes   = 1024
)

// redactMemoryText enforces the size budget after redaction. Redaction markers
// expand the serialized bytes, so a raw payload that fits can overflow once a
// secret is replaced. Oversized redacted text would be silently truncated by
// Get/History (substr) and rejected downstream by fact evidence, embedding and
// semantic cache checks. This mirrors factText in facts.go.
func redactMemoryText(content, scope string) (string, string, error) {
	if strings.TrimSpace(content) == "" || len(content) > maxMemoryContentBytes || len(scope) > maxMemoryScopeBytes {
		return "", "", fmt.Errorf("memory must contain 1–65536 bytes and scope at most 1024 bytes")
	}
	r := redact.Redactor{}
	content, scope = r.Text(content), r.Text(scope)
	if len(content) > maxMemoryContentBytes || len(scope) > maxMemoryScopeBytes || !utf8.ValidString(content) || !utf8.ValidString(scope) {
		return "", "", fmt.Errorf("redacted memory exceeds the 64 KiB content or 1 KiB scope budget")
	}
	return content, scope, nil
}

// RedactImportedContent applies the same content policy to memories assembled
// outside Remember (session/turn and agent-extraction imports). Those paths
// build their own INSERT and would otherwise bypass the size budget that
// reads (substr), embeddings, fact evidence and semantic caching rely on.
func RedactImportedContent(content string) (string, error) {
	if strings.TrimSpace(content) == "" || len(content) > maxMemoryContentBytes {
		return "", fmt.Errorf("imported memory must contain 1–65536 bytes")
	}
	content = (redact.Redactor{}).Text(content)
	if len(content) > maxMemoryContentBytes || !utf8.ValidString(content) {
		return "", fmt.Errorf("redacted imported memory exceeds the 64 KiB content budget")
	}
	return content, nil
}

// NewMemoryID allocates a collision-resistant memory ID from crypto/rand,
// checking the database and retrying so an existing ID is never reused. Time
// stamps alone can collide within one nanosecond across concurrent writers.
func NewMemoryID(ctx context.Context, db *sql.DB) (string, error) {
	for attempt := 0; attempt < 5; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			continue
		}
		id := "mem-" + hex.EncodeToString(random[:])
		var exists int
		err := db.QueryRowContext(ctx, "SELECT 1 FROM memories WHERE id=?", id).Scan(&exists)
		if err == sql.ErrNoRows {
			return id, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not allocate a unique memory ID")
}

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
	var err error
	if content, scope, err = redactMemoryText(content, scope); err != nil {
		return err
	}
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
	if _, err := tx.ExecContext(ctx, "INSERT INTO memory_revisions(memory_id,revision,scope,content,recorded_at) VALUES(?,1,?,?,?)", id, scope, content, at); err != nil {
		return err
	}
	return tx.Commit()
}
