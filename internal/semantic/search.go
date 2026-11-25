package semantic

import (
	"context"
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
	SourceDB            string
	ExcludeDocuments    bool
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
	Protocol        int    `json:"protocol"`
	Status          string `json:"status"`
	Model           string `json:"model"`
	ModelDigest     string `json:"model_digest"`
	StaleVectors    int    `json:"stale_vectors"`
	MissingVectors  int    `json:"missing_vectors"`
	IndexedMemories int    `json:"indexed_memories"`
	// SkippedMalformed counts top-ranked memories left out because their
	// provenance exceeds output bounds or contains protected text.
	SkippedMalformed int            `json:"skipped_malformed"`
	Warnings         []string       `json:"warnings,omitempty"`
	ResultTruncated  bool           `json:"result_truncated"`
	Memories         []SearchMemory `json:"memories"`
}

func ValidateModel(model string) error {
	if strings.TrimSpace(model) != model || model == "" || len(model) > 256 || !utf8.ValidString(model) || strings.ContainsFunc(model, unicode.IsControl) || (redact.Redactor{}).Text(model) != model {
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
	if len(in.SourceDB) > 4096 || !utf8.ValidString(in.SourceDB) {
		return fmt.Errorf("the Trace source identity must contain at most 4096 valid UTF-8 bytes")
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

// contentHash binds an embedding to the exact redacted text sent to the model
// plus the redaction algorithm version. The version makes the fingerprint
// change even when the stored memory bytes are identical but the redaction
// algorithm differs between embed and search.
func contentHash(content, version string) string {
	return redact.FingerprintVersion(content, version)
}

// ContentHash returns the cache fingerprint for learned embedding inputs. The
// memory store keeps the same fingerprint in memories.content_hash.
func ContentHash(content string) string {
	return redact.Fingerprint(content)
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

// scoredCandidate is a memory whose cached vector matched during the scan.
type scoredCandidate struct {
	rowid     int64
	id        string
	createdAt int64
	score     float64
}

type candidateRow struct {
	SearchMemory
	content, hash string
}

// candidateDetails reads provenance, content and the current vector
// fingerprint for one batch of ranked candidates.
func candidateDetails(ctx context.Context, db *sql.DB, identity []any, batch []scoredCandidate) (map[int64]candidateRow, error) {
	args := append([]any{}, identity...)
	for _, c := range batch {
		args = append(args, c.rowid)
	}
	rows, err := db.QueryContext(ctx, `SELECT m.rowid,substr(m.id,1,257),substr(m.scope,1,257),substr(COALESCE(m.source,''),1,257),substr(COALESCE(m.session_id,''),1,257),substr(COALESCE(m.turn_id,''),1,257),m.created_at,
		substr(CAST(m.content AS BLOB),1,65537),COALESCE(e.content_hash,'')
		FROM memories m LEFT JOIN semantic_embeddings e ON e.memory_id=m.id AND e.endpoint=? AND e.model=? AND e.digest=?
		WHERE m.rowid IN (`+strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	details := map[int64]candidateRow{}
	for rows.Next() {
		var rowid int64
		var row candidateRow
		var content []byte
		if err := rows.Scan(&rowid, &row.ID, &row.Scope, &row.Source, &row.SessionID, &row.TurnID, &row.CreatedAt, &content, &row.hash); err != nil {
			return nil, err
		}
		row.content = string(content)
		details[rowid] = row
	}
	return details, rows.Err()
}

// validProvenance reports whether every provenance field fits the semantic
// output bounds and is already a redaction fixed point.
func validProvenance(item SearchMemory) bool {
	for _, value := range []string{item.ID, item.Scope, item.Source, item.SessionID, item.TurnID} {
		if len(value) > 256 || !utf8.ValidString(value) || (redact.Redactor{}).Text(value) != value {
			return false
		}
	}
	return true
}

func searchFilter(in SearchRequest) (string, []any) {
	filter, params := "", []any{}
	if in.Scope != "" {
		filter += " AND m.scope=?"
		params = append(params, in.Scope)
	}
	if in.ExcludeDocuments {
		filter += " AND COALESCE(m.source,'')!='document'"
	}
	if in.SourceDB != "" {
		localSources := "('manual','mcp','document')"
		if in.ExcludeDocuments {
			localSources = "('manual','mcp')"
		}
		filter += " AND (m.source IN " + localSources + " OR EXISTS(SELECT 1 FROM memory_imports i WHERE i.memory_id=m.id AND i.source_db=?))"
		params = append(params, in.SourceDB)
	}
	return filter, params
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
		filter, params := searchFilter(in)
		query := "SELECT count(*) FROM (SELECT 1 FROM memories m WHERE 1=1" + filter
		query += " LIMIT 100001)"
		if err := db.QueryRowContext(ctx, query, params...).Scan(&out.MissingVectors); err != nil {
			return out, err
		}
		if out.MissingVectors > 100000 {
			return out, fmt.Errorf("semantic scan exceeds 100000 memories; choose a narrower scope")
		}
		return out, ctx.Err() // Legacy databases are not migrated by a read.
	}
	// Memories written through the store carry the fingerprint of their
	// content, so cached vectors are matched in SQL and content is read only
	// for rows without one (legacy or externally written) and for the final
	// top results. A database not yet migrated has no column at all.
	var hashColumn int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('memories') WHERE name='content_hash'").Scan(&hashColumn); err != nil {
		return out, err
	}
	memoryHash := "NULL"
	if hashColumn > 0 {
		memoryHash = "m.content_hash"
	}
	identity := []any{provider.Origin(), in.Model, digest}
	query := `SELECT m.rowid,substr(m.id,1,257),m.created_at,` + memoryHash + `,COALESCE(e.content_hash,''),COALESCE(e.dim,0),
		CASE WHEN ` + memoryHash + ` IS NULL THEN substr(CAST(m.content AS BLOB),1,65537) END,
		CASE WHEN ` + memoryHash + ` IS NULL OR ` + memoryHash + `=e.content_hash THEN substr(e.vector,1,32769) END
		FROM memories m LEFT JOIN semantic_embeddings e ON e.memory_id=m.id AND e.endpoint=? AND e.model=? AND e.digest=? WHERE 1=1`
	filter, selected := searchFilter(in)
	query += filter + " LIMIT 100001"
	rows, err := db.QueryContext(ctx, query, append(append([]any{}, identity...), selected...)...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	candidates := []scoredCandidate{}
	scanned := 0
	for rows.Next() {
		scanned++
		if scanned > 100000 {
			return out, fmt.Errorf("semantic scan exceeds 100000 memories; choose a narrower scope")
		}
		var c scoredCandidate
		var stored sql.NullString
		var hash string
		var dim int
		var content, encoded []byte
		if err := rows.Scan(&c.rowid, &c.id, &c.createdAt, &stored, &hash, &dim, &content, &encoded); err != nil {
			return out, err
		}
		if hash == "" {
			out.MissingVectors++
			continue
		}
		if stored.Valid && stored.String != hash {
			out.StaleVectors++
			continue
		}
		if !stored.Valid && (len(content) > 65536 || !utf8.Valid(content) || hash != ContentHash(string(content))) {
			out.StaleVectors++
			continue
		}
		if dim != len(vectors[0]) || len(encoded) != dim*4 {
			out.StaleVectors++
			continue
		}
		vector := embed.Decode(encoded, dim)
		if !validVector(vector) {
			out.StaleVectors++
			continue
		}
		c.score = math.Max(-1, math.Min(1, embed.Cosine(vectors[0], vector)))
		out.IndexedMemories++
		if c.score >= in.MinScore {
			candidates = append(candidates, c)
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.createdAt != b.createdAt {
			return a.createdAt > b.createdAt
		}
		return a.id < b.id
	})
	// Validate, bound and redact only the rows that would be returned, in
	// rank order. A row whose content no longer matches its vector is stale;
	// a row whose provenance exceeds output bounds or contains protected text
	// is skipped and counted rather than failing the whole query.
	for start := 0; start < len(candidates) && len(out.Memories) < in.Limit; {
		end := min(len(candidates), start+in.Limit-len(out.Memories)+8)
		batch := candidates[start:end]
		start = end
		details, err := candidateDetails(ctx, db, identity, batch)
		if err != nil {
			return out, err
		}
		for _, c := range batch {
			if len(out.Memories) == in.Limit {
				break
			}
			row, ok := details[c.rowid]
			if !ok || row.hash == "" || len(row.content) > 65536 || !utf8.ValidString(row.content) || row.hash != ContentHash(row.content) {
				// Changed or removed since the scan, or a stored fingerprint
				// that no longer matches (for example after a redaction
				// algorithm change).
				out.StaleVectors++
				out.IndexedMemories--
				continue
			}
			item := row.SearchMemory
			item.Score = c.score
			if !validProvenance(item) {
				out.SkippedMalformed++
				continue
			}
			item.Content, item.ContentTruncated = snippet(row.content)
			out.Memories = append(out.Memories, item)
		}
	}
	if out.SkippedMalformed > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf("skipped %d memories whose provenance exceeds output bounds or contains protected text", out.SkippedMalformed))
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
