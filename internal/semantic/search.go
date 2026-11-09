package semantic

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/UseTrace-SI/trace-brain/internal/embed"
	"github.com/UseTrace-SI/trace-brain/internal/redact"
)

type Provider interface {
	Origin() string
	Digest(context.Context, string) (string, error)
	Embed(context.Context, string, []string) ([][]float32, error)
	Close()
}
type Factory func(string) (Provider, error)

type SearchRequest struct {
	Query, Scope, Model string
	Limit               int
	MinScore            float64
}
type SearchMemory struct {
	ID               string  `json:"id"`
	Scope            string  `json:"scope"`
	Content          string  `json:"content"`
	Source           string  `json:"source"`
	SessionID        string  `json:"session_id,omitempty"`
	TurnID           string  `json:"turn_id,omitempty"`
	CreatedAt        int64   `json:"created_at"`
	Score            float64 `json:"score"`
	ContentTruncated bool    `json:"content_truncated"`
}
type SearchResponse struct {
	Protocol        int            `json:"protocol"`
	Status          string         `json:"status"`
	Model           string         `json:"model"`
	ModelDigest     string         `json:"model_digest"`
	StaleVectors    int            `json:"stale_vectors"`
	MissingVectors  int            `json:"missing_vectors"`
	IndexedMemories int            `json:"indexed_memories"`
	ResultTruncated bool           `json:"result_truncated"`
	Memories        []SearchMemory `json:"memories"`
}

func ValidateModel(model string) error {
	if strings.TrimSpace(model) != model || model == "" || len(model) > 256 || !utf8.ValidString(model) || strings.ContainsFunc(model, unicode.IsControl) {
		return fmt.Errorf("an exact installed model name of 1–256 bytes without control characters is required")
	}
	return nil
}
func ValidateSearch(in SearchRequest) error {
	if err := ValidateModel(in.Model); err != nil {
		return err
	}
	if strings.TrimSpace(in.Query) == "" || len(in.Query) > 4096 || !utf8.ValidString(in.Query) {
		return fmt.Errorf("query must contain 1–4096 valid UTF-8 bytes")
	}
	if len(in.Scope) > 256 || !utf8.ValidString(in.Scope) {
		return fmt.Errorf("scope must contain at most 256 valid UTF-8 bytes")
	}
	if in.Limit < 1 || in.Limit > 50 {
		return fmt.Errorf("limit must be 1–50")
	}
	if math.IsNaN(in.MinScore) || math.IsInf(in.MinScore, 0) || in.MinScore < -1 || in.MinScore > 1 {
		return fmt.Errorf("min-score must be between -1 and 1")
	}
	return nil
}
func validDigest(digest string) bool {
	_, err := hex.DecodeString(digest)
	return err == nil && len(digest) == 64 && digest == strings.ToLower(digest)
}
func validVector(vector []float32) bool {
	if len(vector) == 0 || len(vector) > 8192 {
		return false
	}
	var norm float64
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return false
		}
		norm += float64(value) * float64(value)
	}
	return norm > 0
}
func snippet(content string) (string, bool) {
	content = (redact.Redactor{}).Text(content)
	if len(content) <= 2048 {
		return content, false
	}
	end := 2048
	for !utf8.RuneStart(content[end]) {
		end--
	}
	return content[:end], true
}

// Search never changes memory/cache state. Inference happens before opening the
// bounded SQLite read; endpoint/model/digest/content identities must all match.
func Search(ctx context.Context, db *sql.DB, provider Provider, in SearchRequest) (SearchResponse, error) {
	out := SearchResponse{Protocol: 1, Status: "needs_embeddings", Model: in.Model, Memories: []SearchMemory{}}
	if err := ValidateSearch(in); err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	digest, err := provider.Digest(ctx, in.Model)
	if err != nil {
		return out, err
	}
	if !validDigest(digest) {
		return out, fmt.Errorf("model digest is missing or invalid")
	}
	out.ModelDigest = digest
	vectors, err := provider.Embed(ctx, in.Model, []string{(redact.Redactor{}).Text(in.Query)})
	if err != nil {
		return out, err
	}
	if len(vectors) != 1 || !validVector(vectors[0]) {
		return out, fmt.Errorf("model returned an invalid query embedding")
	}
	current, err := provider.Digest(ctx, in.Model)
	if err != nil {
		return out, err
	}
	if current != digest {
		return out, fmt.Errorf("model digest changed while embedding the query")
	}
	var exists int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='semantic_embeddings'").Scan(&exists); err != nil {
		return out, err
	}
	if exists == 0 {
		query := "SELECT count(*) FROM (SELECT 1 FROM memories"
		params := []any{}
		if in.Scope != "" {
			query += " WHERE scope=?"
			params = append(params, in.Scope)
		}
		query += " LIMIT 100001)"
		if err := db.QueryRowContext(ctx, query, params...).Scan(&out.MissingVectors); err != nil {
			return out, err
		}
		if out.MissingVectors > 100000 {
			return out, fmt.Errorf("semantic scan exceeds 100000 memories; choose a narrower scope")
		}
		return out, ctx.Err() // Legacy databases are not migrated by a read.
	}
	query := `SELECT substr(m.id,1,257),substr(m.scope,1,257),substr(COALESCE(m.source,''),1,257),substr(COALESCE(m.session_id,''),1,257),substr(COALESCE(m.turn_id,''),1,257),m.created_at,substr(CAST(m.content AS BLOB),1,65537),COALESCE(e.content_hash,''),COALESCE(e.dim,0),substr(e.vector,1,32769) FROM memories m LEFT JOIN semantic_embeddings e ON e.memory_id=m.id AND e.endpoint=? AND e.model=? AND e.digest=? WHERE 1=1`
	params := []any{provider.Origin(), in.Model, digest}
	if in.Scope != "" {
		query += " AND m.scope=?"
		params = append(params, in.Scope)
	}
	query += " ORDER BY m.id LIMIT 100001"
	rows, err := db.QueryContext(ctx, query, params...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	scanned := 0
	for rows.Next() {
		scanned++
		if scanned > 100000 {
			return out, fmt.Errorf("semantic scan exceeds 100000 memories; choose a narrower scope")
		}
		var item SearchMemory
		var content, hash string
		var dim int
		var encoded []byte
		if err := rows.Scan(&item.ID, &item.Scope, &item.Source, &item.SessionID, &item.TurnID, &item.CreatedAt, &content, &hash, &dim, &encoded); err != nil {
			return out, err
		}
		if hash == "" {
			out.MissingVectors++
			continue
		}
		if len(content) > 65536 || !utf8.ValidString(content) || hash != fmt.Sprintf("%x", sha256.Sum256([]byte(content))) || dim != len(vectors[0]) || len(encoded) != dim*4 {
			out.StaleVectors++
			continue
		}
		vector := embed.Decode(encoded, dim)
		if !validVector(vector) {
			out.StaleVectors++
			continue
		}
		item.Score = math.Max(-1, math.Min(1, embed.Cosine(vectors[0], vector)))
		out.IndexedMemories++
		if item.Score < in.MinScore {
			continue
		}
		for _, value := range []string{item.ID, item.Scope, item.Source, item.SessionID, item.TurnID} {
			if len(value) > 256 || !utf8.ValidString(value) || (redact.Redactor{}).Text(value) != value {
				return out, fmt.Errorf("memory provenance exceeds semantic output bounds or contains protected text")
			}
		}
		item.Content, item.ContentTruncated = snippet(content)
		position := sort.Search(len(out.Memories), func(i int) bool {
			previous := out.Memories[i]
			return previous.Score < item.Score || (previous.Score == item.Score && (previous.CreatedAt < item.CreatedAt || (previous.CreatedAt == item.CreatedAt && previous.ID > item.ID)))
		})
		if position < in.Limit {
			out.Memories = append(out.Memories, SearchMemory{})
			copy(out.Memories[position+1:], out.Memories[position:])
			out.Memories[position] = item
			if len(out.Memories) > in.Limit {
				out.Memories = out.Memories[:in.Limit]
			}
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if out.IndexedMemories > 0 {
		out.Status = "ready"
	}
	for {
		encoded, err := json.Marshal(out)
		if err != nil {
			return out, err
		}
		if len(encoded) <= 64<<10 {
			break
		}
		if len(out.Memories) == 0 {
			return out, fmt.Errorf("semantic result metadata exceeds 64 KiB")
		}
		out.ResultTruncated = true
		out.Memories = out.Memories[:len(out.Memories)-1]
	}
	return out, ctx.Err()
}
