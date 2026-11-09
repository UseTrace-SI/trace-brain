package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/UseTrace-SI/trace-brain/internal/embed"
	"github.com/UseTrace-SI/trace-brain/internal/redact"
)

type DocumentInput struct{ Path, Fingerprint, Content string }
type DocumentSync struct {
	Imported           int  `json:"imported"`
	Unchanged          int  `json:"unchanged"`
	Missing            int  `json:"missing"`
	Pruned             int  `json:"pruned"`
	Chunks             int  `json:"chunks"`
	SourceFilesChanged bool `json:"source_files_changed"`
}
type DocumentRecord struct {
	Path        string `json:"path"`
	Fingerprint string `json:"fingerprint"`
	Scope       string `json:"scope"`
	Chunks      int    `json:"chunks"`
	ImportedAt  int64  `json:"imported_at"`
}

func documentChunks(content string) []string {
	content = (redact.Redactor{}).Text(content)
	runes := []rune(content)
	chunks := []string{}
	for len(runes) > 0 {
		n := 4096
		if len(runes) < n {
			n = len(runes)
		}
		part := strings.TrimSpace(string(runes[:n]))
		if part != "" {
			chunks = append(chunks, part)
		}
		runes = runes[n:]
	}
	return chunks
}

// SyncDocuments only writes derived local records. Source files are never
// modified, and missing documents are removed only with explicit pruning.
func SyncDocuments(ctx context.Context, db *sql.DB, root, scope string, documents []DocumentInput, prune bool, at int64) (DocumentSync, error) {
	result := DocumentSync{}
	if root == "" || len(root) > 4096 || (redact.Redactor{}).Text(root) != root || len(scope) > 1024 || len(documents) > 512 {
		return result, fmt.Errorf("invalid document source or budget")
	}
	scope = (redact.Redactor{}).Text(scope)
	if len(scope) > 1024 {
		return result, fmt.Errorf("redacted scope exceeds budget")
	}
	seen := map[string]bool{}
	total, chunksTotal := 0, 0
	prepared := make([][]string, len(documents))
	for i, document := range documents {
		if document.Path == "" || len(document.Path) > 512 || seen[document.Path] || len(document.Fingerprint) != 64 || len(document.Content) > 256*1024 || !utf8.ValidString(document.Content) {
			return result, fmt.Errorf("invalid document")
		}
		if (redact.Redactor{}).Text(document.Path) != document.Path {
			return result, fmt.Errorf("document path contains credentials")
		}
		seen[document.Path] = true
		total += len(document.Content)
		prepared[i] = documentChunks(document.Content)
		chunksTotal += len(prepared[i])
	}
	if total > 16*1024*1024 || chunksTotal > 4096 {
		return result, fmt.Errorf("document corpus exceeds budget")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	records, err := listDocuments(ctx, tx, root)
	if err != nil {
		return result, err
	}
	remove := func(path string) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM memories WHERE id IN (SELECT memory_id FROM document_chunks WHERE root=? AND path=?)", root, path); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM document_sources WHERE root=? AND path=?", root, path)
		return err
	}
	kept := 0
	for _, record := range records {
		if !seen[record.Path] {
			result.Missing++
			if prune {
				if err := remove(record.Path); err != nil {
					return result, err
				}
				result.Pruned++
				continue
			}
		}
		kept++
	}
	existing := map[string]DocumentRecord{}
	for _, record := range records {
		existing[record.Path] = record
	}
	for _, document := range documents {
		if _, ok := existing[document.Path]; !ok {
			kept++
		}
	}
	if kept > 512 {
		return result, fmt.Errorf("source retains more than 512 documents; prune missing records explicitly")
	}
	for i, document := range documents {
		previous, exists := existing[document.Path]
		var actual int
		if exists {
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM document_chunks WHERE root=? AND path=?", root, document.Path).Scan(&actual); err != nil {
				return result, err
			}
		}
		result.Chunks += len(prepared[i])
		matches := false
		if exists && previous.Fingerprint == document.Fingerprint && previous.Scope == scope && previous.Chunks == len(prepared[i]) && actual == previous.Chunks {
			matches, err = documentContentsMatch(ctx, tx, root, document.Path, scope, prepared[i])
			if err != nil {
				return result, err
			}
		}
		if matches {
			result.Unchanged++
			continue
		}
		if err := remove(document.Path); err != nil {
			return result, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO document_sources VALUES(?,?,?,?,?,?)", root, document.Path, document.Fingerprint, scope, len(prepared[i]), at); err != nil {
			return result, err
		}
		for ordinal, chunk := range prepared[i] {
			id := fmt.Sprintf("doc-%x", sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", root, document.Path, ordinal))))
			content := "Document: " + document.Path + "\n" + chunk
			if _, err := tx.ExecContext(ctx, "INSERT INTO memories(id,scope,content,source,created_at) VALUES(?,?,?,'document',?)", id, scope, content, at); err != nil {
				return result, err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO embeddings(memory_id,dim,vector) VALUES(?,?,?)", id, embed.Dim, embed.Encode(embed.Embed(content))); err != nil {
				return result, err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO memory_revisions VALUES(?,1,?,?,?)", id, scope, content, at); err != nil {
				return result, err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO document_chunks VALUES(?,?,?,?)", root, document.Path, ordinal, id); err != nil {
				return result, err
			}
		}
		result.Imported++
	}
	return result, tx.Commit()
}

// Fingerprints alone cannot detect a changed redaction/chunking implementation
// or a forgotten/changed derived record. Compare the expected local projection.
func documentContentsMatch(ctx context.Context, tx *sql.Tx, root, path, scope string, chunks []string) (bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT c.ordinal,m.content,m.scope,COALESCE(m.source,'') FROM document_chunks c JOIN memories m ON m.id=c.memory_id WHERE c.root=? AND c.path=? ORDER BY c.ordinal", root, path)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		var ordinal int
		var content, storedScope, source string
		if err := rows.Scan(&ordinal, &content, &storedScope, &source); err != nil {
			return false, err
		}
		if index >= len(chunks) || ordinal != index || content != "Document: "+path+"\n"+chunks[index] || scope != storedScope || source != "document" {
			return false, nil
		}
		index++
	}
	return index == len(chunks), rows.Err()
}

type documentQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func listDocuments(ctx context.Context, db documentQuery, root string) ([]DocumentRecord, error) {
	rows, err := db.QueryContext(ctx, "SELECT path,fingerprint,scope,chunk_count,imported_at FROM document_sources WHERE root=? ORDER BY path LIMIT 513", root)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []DocumentRecord{}
	for rows.Next() {
		var row DocumentRecord
		if err := rows.Scan(&row.Path, &row.Fingerprint, &row.Scope, &row.Chunks, &row.ImportedAt); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if len(result) > 512 {
		return nil, fmt.Errorf("document record budget exceeded")
	}
	return result, rows.Err()
}
func ListDocuments(ctx context.Context, db *sql.DB, root string) ([]DocumentRecord, error) {
	return listDocuments(ctx, db, root)
}
