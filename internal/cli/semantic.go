package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/embed"
	"github.com/UseTrace-SI/trace-brain/internal/redact"
	"github.com/UseTrace-SI/trace-brain/internal/semantic"
	"github.com/spf13/cobra"
)

func textFingerprint(text string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(text))) }

type modelProvider interface {
	Origin() string
	Digest(context.Context, string) (string, error)
	Embed(context.Context, string, []string) ([][]float32, error)
	Close()
}
type modelFactory func(string) (modelProvider, error)

func localModelProvider(endpoint string) (modelProvider, error) { return semantic.NewOllama(endpoint) }

func embedCmd(factory modelFactory) *cobra.Command {
	var model, endpoint string
	var limit, offset int
	var force bool
	cmd := &cobra.Command{Use: "embed", Short: "Explicitly index memories with an installed local Ollama embedding model", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if limit < 1 || limit > 1000 || offset < 0 || offset > 1_000_000 {
			return fmt.Errorf("limit must be 1–1000 and offset 0–1000000")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
		defer cancel()
		provider, err := factory(endpoint)
		if err != nil {
			return err
		}
		defer provider.Close()
		digest, err := provider.Digest(ctx, model)
		if err != nil {
			return err
		}
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		rows, err := db.QueryContext(ctx, `SELECT m.id,substr(m.content,1,65537),COALESCE(e.content_hash,'') FROM memories m LEFT JOIN semantic_embeddings e ON e.memory_id=m.id AND e.endpoint=? AND e.model=? AND e.digest=? ORDER BY m.id LIMIT ? OFFSET ?`, provider.Origin(), model, digest, limit+1, offset)
		if err != nil {
			return err
		}
		type input struct{ id, content, hash string }
		inputs := []input{}
		limited := false
		selected, unchanged, totalBytes := 0, 0, 0
		for rows.Next() {
			if selected == limit {
				limited = true
				break
			}
			selected++
			var item input
			var oldHash string
			if err := rows.Scan(&item.id, &item.content, &oldHash); err != nil {
				rows.Close()
				return err
			}
			if len(item.content) > 65536 {
				rows.Close()
				return fmt.Errorf("memory exceeds the 64 KiB embedding input budget")
			}
			item.hash = textFingerprint(item.content)
			if !force && item.hash == oldHash {
				unchanged++
				continue
			}
			totalBytes += len(item.content)
			if totalBytes > 16<<20 {
				rows.Close()
				return fmt.Errorf("selected memory text exceeds 16 MiB; use a smaller limit")
			}
			inputs = append(inputs, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		embedded, changed, dim := 0, 0, 0
		for start := 0; start < len(inputs); start += 16 {
			end := start + 16
			if end > len(inputs) {
				end = len(inputs)
			}
			batch := inputs[start:end]
			texts := make([]string, len(batch))
			for i, item := range batch {
				texts[i] = (redact.Redactor{}).Text(item.content)
			}
			vectors, err := provider.Embed(ctx, model, texts)
			if err != nil {
				return err
			}
			current, err := provider.Digest(ctx, model)
			if err != nil {
				return err
			}
			if current != digest {
				return fmt.Errorf("model digest changed during indexing; refresh with a stable model")
			}
			if dim == 0 {
				dim = len(vectors[0])
			}
			if dim != len(vectors[0]) {
				return fmt.Errorf("model dimensions changed between batches")
			}
			// No transaction is held during model inference. Each batch commits
			// independently, and changed/deleted source rows are never cached.
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			for i, item := range batch {
				result, err := tx.ExecContext(ctx, `INSERT INTO semantic_embeddings(memory_id,endpoint,model,digest,content_hash,dim,vector) SELECT id,?,?,?,?,?,? FROM memories WHERE id=? AND content=? ON CONFLICT(memory_id,endpoint,model,digest) DO UPDATE SET content_hash=excluded.content_hash,dim=excluded.dim,vector=excluded.vector`, provider.Origin(), model, digest, item.hash, dim, embed.Encode(vectors[i]), item.id, item.content)
				if err != nil {
					tx.Rollback()
					return err
				}
				count, err := result.RowsAffected()
				if err != nil {
					tx.Rollback()
					return err
				}
				if count == 0 {
					changed++
				} else {
					embedded++
				}
			}
			if err := tx.Commit(); err != nil {
				return err
			}
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"protocol": 1, "model": model, "model_digest": digest, "embedded": embedded, "unchanged": unchanged, "changed_during_inference": changed, "limited": limited, "next_offset": offset + selected})
	}}
	cmd.Flags().StringVar(&model, "model", "", "exact installed Ollama model name (required)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "http://127.0.0.1:11434", "local Ollama origin; remote endpoints and redirects are refused")
	cmd.Flags().IntVar(&limit, "limit", 1000, "maximum selected memories (1–1000)")
	cmd.Flags().IntVar(&offset, "offset", 0, "offset into memories ordered by ID")
	cmd.Flags().BoolVar(&force, "force", false, "recompute vectors even when memory content is unchanged")
	return cmd
}

func semanticCmd(factory modelFactory) *cobra.Command {
	var model, endpoint, scope string
	var limit int
	var minScore float64
	cmd := &cobra.Command{Use: "semantic <query>", Short: "Search learned vectors using an explicitly selected local Ollama model", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		query := strings.Join(args, " ")
		if len(query) > 4096 || strings.TrimSpace(query) == "" {
			return fmt.Errorf("query must contain 1–4096 bytes")
		}
		if limit < 1 || limit > 50 {
			return fmt.Errorf("limit must be 1–50")
		}
		if math.IsNaN(minScore) || math.IsInf(minScore, 0) || minScore < -1 || minScore > 1 {
			return fmt.Errorf("min-score must be between -1 and 1")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
		defer cancel()
		db, err := readMemoryDB()
		if err != nil {
			return err
		}
		defer db.Close()
		provider, err := factory(endpoint)
		if err != nil {
			return err
		}
		defer provider.Close()
		digest, err := provider.Digest(ctx, model)
		if err != nil {
			return err
		}
		vectors, err := provider.Embed(ctx, model, []string{(redact.Redactor{}).Text(query)})
		if err != nil {
			return err
		}
		current, err := provider.Digest(ctx, model)
		if err != nil {
			return err
		}
		if current != digest {
			return fmt.Errorf("model digest changed while embedding the query")
		}
		q := `SELECT m.id,m.scope,substr(m.content,1,2048),COALESCE(m.source,''),COALESCE(m.session_id,''),COALESCE(m.turn_id,''),m.created_at,substr(m.content,1,65537),COALESCE(e.content_hash,''),COALESCE(e.dim,0),substr(e.vector,1,32769) FROM memories m LEFT JOIN semantic_embeddings e ON e.memory_id=m.id AND e.endpoint=? AND e.model=? AND e.digest=? WHERE 1=1`
		params := []any{provider.Origin(), model, digest}
		if scope != "" {
			q += " AND m.scope=?"
			params = append(params, scope)
		}
		q += " ORDER BY m.id"
		rows, err := db.QueryContext(ctx, q, params...)
		if err != nil {
			return err
		}
		type result struct {
			memoryItem
			Score float64 `json:"score"`
		}
		results := []result{}
		stale, valid, missing := 0, 0, 0
		for rows.Next() {
			var item memoryItem
			var content, hash string
			var dim int
			var vector []byte
			if err := rows.Scan(&item.ID, &item.Scope, &item.Content, &item.Source, &item.SessionID, &item.TurnID, &item.CreatedAt, &content, &hash, &dim, &vector); err != nil {
				rows.Close()
				return err
			}
			if hash == "" {
				missing++
				continue
			}
			if len(content) > 65536 || hash != textFingerprint(content) || dim != len(vectors[0]) || len(vector) != dim*4 {
				stale++
				continue
			}
			score := embed.Cosine(vectors[0], embed.Decode(vector, dim))
			if math.IsNaN(score) || math.IsInf(score, 0) {
				stale++
				continue
			}
			valid++
			if score < minScore {
				continue
			}
			position := sort.Search(len(results), func(i int) bool {
				return results[i].Score < score || (results[i].Score == score && (results[i].CreatedAt < item.CreatedAt || (results[i].CreatedAt == item.CreatedAt && results[i].ID > item.ID)))
			})
			if position < limit {
				results = append(results, result{})
				copy(results[position+1:], results[position:])
				results[position] = result{memoryItem: item, Score: score}
				if len(results) > limit {
					results = results[:limit]
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		status := "ready"
		if valid == 0 {
			status = "needs_embeddings"
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"protocol": 1, "status": status, "model": model, "model_digest": digest, "stale_vectors": stale, "missing_vectors": missing, "indexed_memories": valid, "memories": results})
	}}
	cmd.Flags().StringVar(&model, "model", "", "exact installed Ollama model name (required)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "http://127.0.0.1:11434", "local Ollama origin; remote endpoints and redirects are refused")
	cmd.Flags().StringVar(&scope, "scope", "", "exact memory scope filter")
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum returned memories (1–50)")
	cmd.Flags().Float64Var(&minScore, "min-score", -1, "optional cosine similarity threshold (-1–1); default ranks all valid vectors")
	return cmd
}
