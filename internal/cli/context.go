package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/UseTrace-SI/trace-brain/internal/embed"
	learned "github.com/UseTrace-SI/trace-brain/internal/semantic"
	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/spf13/cobra"
)

func refreshCmd() *cobra.Command {
	var source string
	cmd := &cobra.Command{Use: "refresh", Short: "Import Trace history and return protocol-1 JSON", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		started := time.Now()
		if err := importTrace(cmd.Context(), io.Discard, source, "", false); err != nil {
			return err
		}
		// Extraction is bounded and reported; it never fails the import.
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"protocol": 1, "status": "ready", "facts": extractAfterImport(cmd.Context(), started)})
	}}
	cmd.Flags().StringVar(&source, "trace-db", ".trace/trace.db", "Trace history database (read-only)")
	return cmd
}

// This read-only JSON contract is consumed by Trace CLI. It never creates a
// database or runs extraction/agent commands during a context lookup.
type memoryContext struct {
	Protocol  int                 `json:"protocol"`
	Status    string              `json:"status"`
	Memories  []memoryItem        `json:"memories"`
	Retrieval string              `json:"retrieval,omitempty"`
	Learned   *learnedContextInfo `json:"learned,omitempty"`
	// Facts and FactsTruncated appear only when --facts is requested.
	Facts          []contextFact `json:"facts,omitempty"`
	FactsTruncated bool          `json:"facts_truncated,omitempty"`
}

// contextFact is a compact current claim; quotes are capped for context.
type contextFact struct {
	ID         string `json:"id"`
	Subject    string `json:"subject"`
	Predicate  string `json:"predicate"`
	Object     string `json:"object"`
	ValidFrom  int64  `json:"valid_from"`
	ValidTo    *int64 `json:"valid_to,omitempty"`
	Origin     string `json:"origin"`
	Quote      string `json:"quote,omitempty"`
	EvidenceID string `json:"evidence_id"`
	// EvidenceSource is the evidence memory's source (trace-import, manual,
	// mcp, ...), so a consumer can keep local notes out of shared output.
	EvidenceSource string `json:"evidence_source"`
	TurnID         string `json:"turn_id,omitempty"`
}

const maxContextQuoteBytes = 256

func contextFacts(cmd *cobra.Command, db *sql.DB, terms []string, limit int, source string) ([]contextFact, bool, error) {
	facts, truncated, err := store.ContextFacts(cmd.Context(), db, terms, limit, time.Now().UnixMilli(), source)
	if err != nil {
		return nil, false, err
	}
	out := []contextFact{}
	for _, f := range facts {
		f = readFact(f)
		quote := f.Quote
		if len(quote) > maxContextQuoteBytes {
			quote = quote[:maxContextQuoteBytes]
			for !utf8.ValidString(quote) {
				quote = quote[:len(quote)-1]
			}
		}
		out = append(out, contextFact{ID: f.ID, Subject: f.Subject, Predicate: f.Predicate, Object: f.Object, ValidFrom: f.ValidFrom, ValidTo: f.ValidTo, Origin: f.Origin, Quote: quote, EvidenceID: f.EvidenceID, EvidenceSource: f.EvidenceSource, TurnID: f.EvidenceTurnID})
	}
	return out, truncated, nil
}

type memoryItem struct {
	ID               string   `json:"id"`
	Scope            string   `json:"scope"`
	Content          string   `json:"content"`
	Source           string   `json:"source"`
	SessionID        string   `json:"session_id,omitempty"`
	TurnID           string   `json:"turn_id,omitempty"`
	CreatedAt        int64    `json:"created_at"`
	Score            *float64 `json:"score,omitempty"`
	ContentTruncated bool     `json:"content_truncated,omitempty"`
}

func contextCmd(factory modelFactory) *cobra.Command {
	var query, source string
	var limit, factLimit int
	var semantic bool
	options := learnedContextOptions{}
	cmd := &cobra.Command{Use: "context", Short: "Read bounded memory context as protocol-1 JSON", SilenceUsage: true, SilenceErrors: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := options.validate(cmd, query, limit, semantic); err != nil {
			return err
		}
		if factLimit < 0 || factLimit > store.MaxContextFacts || (factLimit > 0 && options.model != "") {
			return fmt.Errorf("--facts must be 0–%d and cannot be combined with --learned-model", store.MaxContextFacts)
		}
		timeout := 2 * time.Second
		if options.model != "" {
			timeout = time.Minute
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
		defer cancel()
		cmd.SetContext(ctx)

		out := memoryContext{Protocol: 1, Status: "missing_index", Memories: []memoryItem{}}
		if options.model != "" {
			out.Retrieval = "learned_local_ollama"
			out.Learned = &learnedContextInfo{Model: options.model, ModelStatus: "not_probed", DocumentsIncluded: options.documents}
		}
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
		// A read never migrates. A schema from a newer release fails the
		// command (Trace reports the companion as failed) rather than being
		// misread as ready.
		if err := store.CheckReadableSchema(cmd.Context(), db); err != nil {
			return err
		}
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
		if options.model != "" {
			return options.write(cmd, db, factory, learned.SearchRequest{Query: query, Model: options.model, Scope: options.scope, SourceDB: source, ExcludeDocuments: !options.documents, Limit: limit, MinScore: options.minScore})
		}
		if limit <= 0 || limit > 50 {
			limit = 20
		}
		// Treat ordinary task text as literal words, never as FTS syntax.
		terms, err := store.QueryTerms(query)
		if err != nil {
			return err
		}
		if strings.TrimSpace(query) != "" && len(terms) == 0 {
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
				if score <= 0 {
					continue
				}
				position := sort.Search(len(candidates), func(i int) bool {
					previous := candidates[i]
					return previous.score < score || (previous.score == score && (previous.item.CreatedAt < item.CreatedAt || (previous.item.CreatedAt == item.CreatedAt && previous.item.ID > item.ID)))
				})
				if position < limit {
					candidates = append(candidates, scored{})
					copy(candidates[position+1:], candidates[position:])
					candidates[position] = scored{item: item, score: score}
					if len(candidates) > limit {
						candidates = candidates[:limit]
					}
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			// Sorted by score desc, then created_at desc, then ID asc.
			if limit > len(candidates) {
				limit = len(candidates)
			}
			for i := 0; i < limit; i++ {
				item := candidates[i].item
				item.Scope, item.Content = readRedacted(item.Scope, item.Content)
				out.Memories = append(out.Memories, item)
			}
		} else {
			results, err := store.Search(cmd.Context(), db, store.SearchOpts{Query: query, SourceDB: source, Limit: limit, RecentWhenEmpty: true})
			if err != nil {
				return err
			}
			// Search re-applies read-time redaction: context output can be
			// published by Trace, so legacy rows must not bypass the policy.
			for _, m := range results {
				out.Memories = append(out.Memories, memoryItem{ID: m.ID, Scope: m.Scope, Content: m.Content, Source: m.Source, SessionID: m.SessionID, TurnID: m.TurnID, CreatedAt: m.CreatedAt, ContentTruncated: m.ContentTruncated})
			}
		}
		if factLimit > 0 {
			if out.Facts, out.FactsTruncated, err = contextFacts(cmd, db, terms, factLimit, source); err != nil {
				return err
			}
		}
		out.Status = "ready"
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}}
	cmd.Flags().StringVar(&query, "query", "", "task words to recall (empty lists recent memories)")
	cmd.Flags().StringVar(&source, "trace-db", "", "scope imported memories to this Trace source (defaults to current project when present)")
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum memories (1-50)")
	cmd.Flags().BoolVar(&semantic, "semantic", false, "rank by offline hashed-word similarity instead of keyword search")
	cmd.Flags().IntVar(&factLimit, "facts", 0, "also include up to N current project claims matching the query (0–20; default off)")
	cmd.Flags().StringVar(&options.model, "learned-model", "", "explicitly query this installed local Ollama model instead of offline retrieval")
	cmd.Flags().StringVar(&options.endpoint, "learned-endpoint", "http://127.0.0.1:11434", "loopback Ollama origin for explicit learned context")
	cmd.Flags().StringVar(&options.scope, "learned-scope", "", "exact memory scope for learned context")
	cmd.Flags().Float64Var(&options.minScore, "learned-min-score", -1, "minimum learned cosine score (-1–1)")
	cmd.Flags().BoolVar(&options.documents, "learned-include-documents", false, "include explicitly ingested local documents in learned context")
	return cmd
}
