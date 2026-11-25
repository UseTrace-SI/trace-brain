package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/spf13/cobra"
)

type workspaceMemory struct {
	Database string `json:"database"`
	Rank     int    `json:"rank"`
	memoryItem
}
type workspaceSource struct {
	Database string `json:"database"`
	Status   string `json:"status"`
}

func workspaceCmd() *cobra.Command {
	root := &cobra.Command{Use: "workspace", Short: "Read explicitly selected project memories without copying them"}
	var paths []string
	var limit int
	var scope string
	query := &cobra.Command{Use: "query <words>", Short: "Federate bounded keyword retrieval across local databases", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if len(paths) == 0 || len(paths) > 16 {
			return fmt.Errorf("select 1–16 --databases paths")
		}
		if limit < 1 || limit > 50 {
			return fmt.Errorf("limit must be 1–50")
		}
		text := strings.Join(args, " ")
		if _, err := store.QueryTerms(text); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
		defer cancel()
		items := []workspaceMemory{}
		sources := []workspaceSource{}
		seen := map[string]bool{}
		for _, path := range paths {
			if len(path) > 4096 {
				return fmt.Errorf("database path exceeds 4096 bytes")
			}
			path, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			if canonical, e := filepath.EvalSymlinks(path); e == nil {
				path = canonical
			}
			if seen[path] {
				continue
			}
			seen[path] = true
			source := workspaceSource{Database: path, Status: "ready"}
			db, err := openReadOnlyMemory(path)
			if err != nil {
				source.Status = "unavailable"
				sources = append(sources, source)
				continue
			}
			results, err := store.Search(ctx, db, store.SearchOpts{Query: text, Scope: scope, Limit: limit})
			db.Close()
			ranked := []workspaceMemory{}
			for _, m := range results {
				item := memoryItem{ID: m.ID, Scope: m.Scope, Content: m.Content, Source: m.Source, SessionID: m.SessionID, TurnID: m.TurnID, CreatedAt: m.CreatedAt}
				ranked = append(ranked, workspaceMemory{Database: path, Rank: len(ranked) + 1, memoryItem: item})
			}
			if err != nil {
				source.Status = "unavailable"
			} else {
				items = append(items, ranked...)
			}
			sources = append(sources, source)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// BM25 magnitudes from different corpora are not comparable. Use each
		// source's rank, with deterministic recency/database/ID ties.
		sort.Slice(items, func(i, j int) bool {
			a, b := items[i], items[j]
			if a.Rank != b.Rank {
				return a.Rank < b.Rank
			}
			if a.CreatedAt != b.CreatedAt {
				return a.CreatedAt > b.CreatedAt
			}
			if a.Database != b.Database {
				return a.Database < b.Database
			}
			return a.ID < b.ID
		})
		if len(items) > limit {
			items = items[:limit]
		}
		partial := false
		for _, source := range sources {
			partial = partial || source.Status != "ready"
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"protocol": 1, "ranking": "source_rank_then_recency", "partial": partial, "sources": sources, "memories": items})
	}}
	query.Flags().StringSliceVar(&paths, "databases", nil, "explicit memory database paths (repeatable or comma-separated)")
	query.Flags().IntVar(&limit, "limit", 20, "maximum combined memories (1–50)")
	query.Flags().StringVar(&scope, "scope", "", "exact memory scope filter")
	root.AddCommand(query)
	return root
}
