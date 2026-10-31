package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/UseTrace-SI/trace-brain/internal/embed"
	"github.com/spf13/cobra"
)

func refreshCmd() *cobra.Command {
	var source string
	cmd := &cobra.Command{Use: "refresh", Short: "Import Trace history and return protocol-1 JSON", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := importTrace(cmd.Context(), io.Discard, source, "", false); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"protocol": 1, "status": "ready"})
	}}
	cmd.Flags().StringVar(&source, "trace-db", ".trace/trace.db", "Trace history database (read-only)")
	return cmd
}

// This read-only JSON contract is consumed by Trace CLI. It never creates a
// database or runs extraction/agent commands during a context lookup.
type memoryContext struct {
	Protocol int          `json:"protocol"`
	Status   string       `json:"status"`
	Memories []memoryItem `json:"memories"`
}

type memoryItem struct {
	ID        string `json:"id"`
	Scope     string `json:"scope"`
	Content   string `json:"content"`
	Source    string `json:"source"`
	SessionID string `json:"session_id,omitempty"`
	TurnID    string `json:"turn_id,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

func contextCmd() *cobra.Command {
	var query, source string
	var limit int
	var semantic bool
	cmd := &cobra.Command{Use: "context", Short: "Read bounded memory context as protocol-1 JSON", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		out := memoryContext{Protocol: 1, Status: "missing_index", Memories: []memoryItem{}}
		if len(query) > 4096 {
			return fmt.Errorf("context query exceeds 4096 bytes")
		}
		path, err := filepath.Abs(dbPath)
		if err != nil {
			return err
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
		} else if err != nil {
			return err
		}
		u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
		db, err := sql.Open("sqlite", u.String())
		if err != nil {
			return err
		}
		defer db.Close()
		if source == "" {
			if _, err := os.Stat(".trace/trace.db"); err == nil {
				source = ".trace/trace.db"
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		if source != "" {
			source, err = filepath.Abs(source)
			if err != nil {
				return err
			}
			source, err = filepath.EvalSymlinks(source)
			if err != nil {
				return err
			}
			var imports int
			if err := db.QueryRowContext(cmd.Context(), "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='memory_imports'").Scan(&imports); err != nil {
				return err
			}
			if imports == 0 {
				out.Status = "needs_refresh"
				return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
			}
		}
		if limit <= 0 || limit > 50 {
			limit = 20
		}
		// Treat ordinary task text as literal words, never as FTS syntax.
		words := strings.FieldsFunc(query, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' })
		if len(words) > 16 {
			words = words[:16]
		}
		for i := range words {
			words[i] = `"` + words[i] + `"`
		}
		if strings.TrimSpace(query) != "" && len(words) == 0 {
			out.Status = "ready"
			return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
		}
		if semantic && strings.TrimSpace(query) != "" {
			q := `SELECT m.id,m.scope,substr(m.content,1,2048),COALESCE(m.source,''),COALESCE(m.session_id,''),COALESCE(m.turn_id,''),m.created_at,e.vector
				FROM memories m LEFT JOIN embeddings e ON e.memory_id=m.id WHERE 1=1`
			var args []any
			if source != "" {
				q += ` AND (m.source IN ('manual','mcp') OR EXISTS(SELECT 1 FROM memory_imports i WHERE i.memory_id=m.id AND i.source_db=?))`
				args = append(args, source)
			}
			rows, err := db.QueryContext(cmd.Context(), q, args...)
			if err != nil {
				return err
			}
			type scored struct {
				item  memoryItem
				score float64
			}
			qv := embed.Embed(query)
			var candidates []scored
			for rows.Next() {
				var item memoryItem
				var vec []byte
				if err := rows.Scan(&item.ID, &item.Scope, &item.Content, &item.Source, &item.SessionID, &item.TurnID, &item.CreatedAt, &vec); err != nil {
					rows.Close()
					return err
				}
				score := 0.0
				if v := embed.Decode(vec, embed.Dim); v != nil {
					score = embed.Cosine(qv, v)
				}
				candidates = append(candidates, scored{item: item, score: score})
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			// Sort by score desc, then created_at desc.
			for i := 1; i < len(candidates); i++ {
				for j := i; j > 0 && candidates[j-1].score < candidates[j].score; j-- {
					candidates[j-1], candidates[j] = candidates[j], candidates[j-1]
				}
			}
			if limit > len(candidates) {
				limit = len(candidates)
			}
			for i := 0; i < limit; i++ {
				out.Memories = append(out.Memories, candidates[i].item)
			}
		} else {
			q := `SELECT m.id,m.scope,substr(m.content,1,2048),COALESCE(m.source,''),COALESCE(m.session_id,''),COALESCE(m.turn_id,''),m.created_at FROM memories m`
			var args []any
			if len(words) > 0 {
				q += ` JOIN memory_fts f ON f.rowid=m.rowid WHERE memory_fts MATCH ?`
				args = append(args, strings.Join(words, " OR "))
			} else {
				q += ` WHERE 1=1`
			}
			if source != "" {
				q += ` AND (m.source IN ('manual','mcp') OR EXISTS(SELECT 1 FROM memory_imports i WHERE i.memory_id=m.id AND i.source_db=?))`
				args = append(args, source)
			}
			q += ` ORDER BY m.created_at DESC,m.id LIMIT ?`
			args = append(args, limit)
			rows, err := db.QueryContext(cmd.Context(), q, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var item memoryItem
				if err := rows.Scan(&item.ID, &item.Scope, &item.Content, &item.Source, &item.SessionID, &item.TurnID, &item.CreatedAt); err != nil {
					return err
				}
				out.Memories = append(out.Memories, item)
			}
			if err := rows.Err(); err != nil {
				return err
			}
		}
		out.Status = "ready"
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}}
	cmd.Flags().StringVar(&query, "query", "", "task words to recall (empty lists recent memories)")
	cmd.Flags().StringVar(&source, "trace-db", "", "scope imported memories to this Trace source (defaults to current project when present)")
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum memories (1-50)")
	cmd.Flags().BoolVar(&semantic, "semantic", false, "rank by local embedding similarity instead of keyword search")
	return cmd
}
