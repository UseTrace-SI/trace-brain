package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/spf13/cobra"
)

type factTimes struct {
	At, KnownAt  string
	Limit        int
	IncludeStale bool
}

func factTimestamp(value string, fallback int64) (int64, error) {
	if value == "" {
		return fallback, nil
	}
	if len(value) > 64 {
		return 0, fmt.Errorf("fact time exceeds budget")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.UnixMilli() < 0 {
		return 0, fmt.Errorf("fact times must be RFC3339 dates at or after 1970")
	}
	return parsed.UnixMilli(), nil
}
func factOptions(flags factTimes, scope string) (store.FactQuery, error) {
	now := time.Now().UnixMilli()
	at, err := factTimestamp(flags.At, now)
	if err != nil {
		return store.FactQuery{}, err
	}
	known, err := factTimestamp(flags.KnownAt, now)
	if err != nil {
		return store.FactQuery{}, err
	}
	return store.FactQuery{Scope: scope, At: at, KnownAt: known, Limit: flags.Limit, IncludeStale: flags.IncludeStale}, nil
}
func factQueryFlags(cmd *cobra.Command, flags *factTimes, limit int) {
	cmd.Flags().StringVar(&flags.At, "at", "", "query effective validity at this RFC3339 time (default now)")
	cmd.Flags().StringVar(&flags.KnownAt, "known-at", "", "query assertions known at this RFC3339 time (default now)")
	cmd.Flags().IntVar(&flags.Limit, "limit", limit, "maximum returned assertions (1–100)")
	cmd.Flags().BoolVar(&flags.IncludeStale, "include-stale", false, "include claims whose current evidence content has changed")
}
func factsCmd() *cobra.Command {
	var scope string
	parent := &cobra.Command{Use: "facts", Short: "Manage evidence-linked temporal claims; content is not certified as true"}
	parent.PersistentFlags().StringVar(&scope, "scope", "project", "exact local fact scope")
	var evidence, from, until string
	var exclusive bool
	assert := &cobra.Command{Use: "assert SUBJECT PREDICATE OBJECT", Args: cobra.ExactArgs(3), Short: "Record a user-supplied claim linked to a local memory", RunE: func(cmd *cobra.Command, args []string) error {
		now := time.Now().UnixMilli()
		start, err := factTimestamp(from, now)
		if err != nil {
			return err
		}
		var end *int64
		if until != "" {
			value, err := factTimestamp(until, 0)
			if err != nil {
				return err
			}
			end = &value
		}
		if evidence == "" {
			return fmt.Errorf("--evidence requires a local memory ID")
		}
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
		defer cancel()
		fact, err := store.AssertFact(ctx, db, store.Fact{Scope: scope, Subject: args[0], Predicate: args[1], Object: args[2], ValidFrom: start, ValidTo: end, EvidenceID: evidence, Exclusive: exclusive}, now)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"fact": fact, "source_verified": false, "local_only": true})
	}}
	assert.Flags().StringVar(&evidence, "evidence", "", "existing local memory ID (required)")
	assert.Flags().StringVar(&from, "from", "", "valid from this RFC3339 time (default now)")
	assert.Flags().StringVar(&until, "until", "", "exclusive end of validity (empty is open-ended)")
	assert.Flags().BoolVar(&exclusive, "exclusive", false, "reject overlapping different values for this scoped subject/predicate")
	retract := &cobra.Command{Use: "retract ID", Args: cobra.ExactArgs(1), Short: "Retract a claim while preserving earlier recorded-time queries", RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := os.Stat(dbPath); err != nil {
			return fmt.Errorf("memory index is unavailable")
		}
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
		defer cancel()
		if err := store.RetractFact(ctx, db, args[0], time.Now().UnixMilli()); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"id": args[0], "retracted": true, "source_verified": false})
	}}
	var queryTimes factTimes
	var subject, predicate, object string
	query := &cobra.Command{Use: "query", Args: cobra.NoArgs, Short: "Read claims by validity and recorded time, excluding stale evidence by default", RunE: func(cmd *cobra.Command, _ []string) error {
		options, err := factOptions(queryTimes, scope)
		if err != nil {
			return err
		}
		options.Subject = subject
		options.Predicate = predicate
		options.Object = object
		db, err := readMemoryDB()
		if err != nil {
			return err
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
		defer cancel()
		result, err := store.QueryFacts(ctx, db, options)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"result": result, "at": options.At, "known_at": options.KnownAt, "evidence_check": "current_memory_hash", "local_only": true})
	}}
	factQueryFlags(query, &queryTimes, 20)
	query.Flags().StringVar(&subject, "subject", "", "exact subject filter")
	query.Flags().StringVar(&predicate, "predicate", "", "exact predicate filter")
	query.Flags().StringVar(&object, "object", "", "exact object filter")
	var graphTimes factTimes
	var depth int
	var direction string
	graph := &cobra.Command{Use: "graph ENTITY", Args: cobra.ExactArgs(1), Short: "Traverse bounded, scoped claim relationships", RunE: func(cmd *cobra.Command, args []string) error {
		options, err := factOptions(graphTimes, scope)
		if err != nil {
			return err
		}
		if depth < 1 || depth > 8 || !map[string]bool{"out": true, "both": true}[direction] {
			return fmt.Errorf("depth must be 1–8 and direction out or both")
		}
		db, err := readMemoryDB()
		if err != nil {
			return err
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
		defer cancel()
		result, err := traverseFacts(ctx, db, args[0], options, depth, direction)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"result": result, "at": options.At, "known_at": options.KnownAt, "scope": scope, "evidence_check": "current_memory_hash", "local_only": true})
	}}
	factQueryFlags(graph, &graphTimes, 100)
	graph.Flags().IntVar(&depth, "depth", 3, "maximum traversal depth (1–8)")
	graph.Flags().StringVar(&direction, "direction", "out", "relationship direction: out or both")
	parent.AddCommand(assert, retract, query, graph)
	return parent
}

func traverseFacts(ctx context.Context, db *sql.DB, entity string, options store.FactQuery, depth int, direction string) (store.FactResults, error) {
	result := store.FactResults{Facts: []store.Fact{}}
	if options.Limit < 1 || options.Limit > 100 {
		return result, fmt.Errorf("fact graph limit must be 1–100")
	}
	type hop struct {
		entity string
		depth  int
	}
	queue := []hop{{entity, 0}}
	seen := map[string]bool{entity: true}
	edges := map[string]bool{}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.depth >= depth {
			continue
		}
		if len(result.Facts) >= options.Limit {
			result.Truncated = true
			break
		}
		query := options
		query.Limit = options.Limit - len(result.Facts)
		if direction == "both" {
			query.Entity = current.entity
		} else {
			query.Subject = current.entity
		}
		found, err := store.QueryFacts(ctx, db, query)
		if err != nil {
			return result, err
		}
		result.Truncated = result.Truncated || found.Truncated
		result.StaleOmitted += found.StaleOmitted
		for _, fact := range found.Facts {
			if edges[fact.ID] {
				continue
			}
			edges[fact.ID] = true
			result.Facts = append(result.Facts, fact)
			neighbors := []string{fact.Object}
			if direction == "both" {
				neighbors = append(neighbors, fact.Subject)
			}
			for _, next := range neighbors {
				if !seen[next] {
					seen[next] = true
					queue = append(queue, hop{next, current.depth + 1})
				}
			}
		}
	}
	return result, nil
}
