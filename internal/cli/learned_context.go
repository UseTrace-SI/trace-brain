package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/UseTrace-SI/trace-brain/internal/semantic"
	"github.com/spf13/cobra"
)

type learnedContextOptions struct {
	model, endpoint, scope string
	minScore               float64
	documents              bool
}
type learnedContextInfo struct {
	Model             string `json:"model"`
	ModelDigest       string `json:"model_digest,omitempty"`
	ModelStatus       string `json:"model_status"`
	StaleVectors      int    `json:"stale_vectors"`
	MissingVectors    int    `json:"missing_vectors"`
	IndexedMemories   int    `json:"indexed_memories"`
	ResultTruncated   bool   `json:"result_truncated"`
	DocumentsIncluded bool   `json:"documents_included"`
}

func (options learnedContextOptions) validate(cmd *cobra.Command, query string, limit int, hashed bool) error {
	if options.model == "" {
		for _, flag := range []string{"learned-endpoint", "learned-scope", "learned-min-score", "learned-include-documents"} {
			if cmd.Flags().Changed(flag) {
				return fmt.Errorf("--%s requires --learned-model", flag)
			}
		}
		return nil
	}
	if hashed {
		return fmt.Errorf("choose --semantic or --learned-model explicitly; they cannot be combined")
	}
	if err := semantic.ValidateSearch(semantic.SearchRequest{Query: query, Model: options.model, Scope: options.scope, Limit: limit, MinScore: options.minScore}); err != nil {
		return err
	}
	provider, err := semantic.NewOllama(options.endpoint)
	if err != nil {
		return err
	}
	provider.Close() // Configuration validation only, never a service probe.
	return nil
}

func (options learnedContextOptions) write(cmd *cobra.Command, db *sql.DB, factory modelFactory, in semantic.SearchRequest) error {
	provider, err := factory(options.endpoint)
	if err != nil {
		return err
	}
	defer provider.Close()
	configured, err := semantic.NewOllama(options.endpoint)
	if err != nil {
		return err
	}
	expected := configured.Origin()
	configured.Close()
	if provider.Origin() != expected {
		return fmt.Errorf("learned context provider origin differs from the configured local endpoint")
	}
	result, err := semantic.Search(cmd.Context(), db, provider, in)
	if err != nil {
		return err
	}
	out := memoryContext{Protocol: 1, Status: result.Status, Retrieval: "learned_local_ollama", Memories: []memoryItem{}, Learned: &learnedContextInfo{Model: result.Model, ModelDigest: result.ModelDigest, ModelStatus: "queried", StaleVectors: result.StaleVectors, MissingVectors: result.MissingVectors, IndexedMemories: result.IndexedMemories, ResultTruncated: result.ResultTruncated, DocumentsIncluded: options.documents}}
	for _, item := range result.Memories {
		score := item.Score
		out.Memories = append(out.Memories, memoryItem{ID: item.ID, Scope: item.Scope, Content: item.Content, Source: item.Source, SessionID: item.SessionID, TurnID: item.TurnID, CreatedAt: item.CreatedAt, Score: &score, ContentTruncated: item.ContentTruncated})
	}
	// The context envelope adds metadata to Search's bounded payload. Keep the
	// full encoded context bounded too, without dropping higher-ranked evidence.
	for {
		encoded, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if len(encoded) <= 64<<10 {
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
			return err
		}
		if len(out.Memories) == 0 {
			return fmt.Errorf("learned context metadata exceeds 64 KiB")
		}
		out.Memories = out.Memories[:len(out.Memories)-1]
		out.Learned.ResultTruncated = true
	}
}
