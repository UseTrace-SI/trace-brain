package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	"github.com/UseTrace-SI/trace-brain/internal/extract"
	"github.com/UseTrace-SI/trace-brain/internal/redact"
	"github.com/UseTrace-SI/trace-brain/internal/semantic"
	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/spf13/cobra"
)

// automaticExtraction bounds the deterministic pass that follows setup and
// refresh. Trace allows refresh one minute, so a large backlog is processed
// over several refreshes and reported as pending.
var automaticExtraction = store.RulesOptions{Limit: 500, Budget: 15 * time.Second}

type factsSummary struct {
	store.ExtractionReport
	Error string `json:"error,omitempty"`
}

// extractAfterImport runs the bounded rules pass. Its failure never fails the
// memory import that preceded it; callers report it instead.
func extractAfterImport(ctx context.Context) factsSummary {
	db, err := openDB()
	if err != nil {
		return factsSummary{Error: "fact extraction unavailable: " + err.Error()}
	}
	defer db.Close()
	options := automaticExtraction
	options.At = time.Now().UnixMilli()
	report, err := store.ExtractRules(ctx, db, options)
	summary := factsSummary{ExtractionReport: report}
	if err != nil {
		summary.Error = "fact extraction failed: " + err.Error()
	}
	return summary
}

func writeExtractionText(w io.Writer, summary factsSummary) error {
	if summary.Error != "" {
		_, err := fmt.Fprintln(w, summary.Error)
		return err
	}
	_, err := fmt.Fprintf(w, "facts: %d added, %d retracted, %d conflicts skipped from %d memories (%d pending)\n",
		summary.Added, summary.Retracted, summary.Conflicts, summary.Memories, summary.Pending)
	if err == nil && summary.BudgetExhausted {
		_, err = fmt.Fprintf(w, "facts: extraction stopped at the %d-claim budget; forget unused claims to continue\n", store.ExtractionFactLimit)
	}
	return err
}

const maxListedQuoteRunes = 160

// readFact re-applies redaction to displayed claim text, like other read paths.
func readFact(f store.Fact) store.Fact {
	r := redact.Redactor{}
	f.Subject, f.Object, f.Quote = r.Text(f.Subject), r.Text(f.Object), r.Text(f.Quote)
	return f
}

func factTime(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.RFC3339) }

func writeFactsText(w io.Writer, result store.FactResults) error {
	if len(result.Facts) == 0 {
		_, err := fmt.Fprintln(w, "no claims")
		return err
	}
	for _, f := range result.Facts {
		until := "open"
		if f.ValidTo != nil {
			until = factTime(*f.ValidTo)
		}
		origin := f.Origin
		if f.Extractor != "" {
			origin += "/" + f.Extractor
		}
		line := fmt.Sprintf("%s %s %s  [%s → %s]  %s", f.Subject, f.Predicate, f.Object, factTime(f.ValidFrom), until, origin)
		if f.EvidenceStale {
			line += "  stale"
		}
		if f.RetractedAt != nil {
			line += "  retracted"
		}
		evidence := "evidence " + f.EvidenceID
		if f.EvidenceTurnID != "" {
			evidence += " (turn " + f.EvidenceTurnID + ")"
		}
		if f.Quote != "" {
			quote := f.Quote
			if utf8.RuneCountInString(quote) > maxListedQuoteRunes {
				quote = string([]rune(quote)[:maxListedQuoteRunes]) + "…"
			}
			evidence += ": " + fmt.Sprintf("%q", quote)
		}
		if _, err := fmt.Fprintf(w, "%s\n    %s\n    id %s", line, evidence, f.ID); err != nil {
			return err
		}
		if f.SupersededBy != nil {
			if _, err := fmt.Fprintf(w, "; superseded by %s (evidence %s)", f.SupersededBy.FactID, f.SupersededBy.EvidenceID); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	if result.Truncated {
		_, err := fmt.Fprintln(w, "(truncated; narrow with --subject or raise --limit)")
		return err
	}
	return nil
}

// factsListCmd configures `trace-brain facts`: current claims, or with
// --history every value a subject has had.
func factsListCmd(parent *cobra.Command, scope *string) {
	var subject, predicate, at, knownAt string
	var limit int
	var history, includeStale, asJSON bool
	parent.Args = cobra.NoArgs
	parent.RunE = func(cmd *cobra.Command, _ []string) error {
		now := time.Now().UnixMilli()
		validAt, err := factTimestamp(at, now)
		if err != nil {
			return err
		}
		known, err := factTimestamp(knownAt, now)
		if err != nil {
			return err
		}
		if limit < 1 || limit > 100 {
			return fmt.Errorf("limit must be 1–100")
		}
		if history && (subject == "" || cmd.Flags().Changed("at")) {
			return fmt.Errorf("--history requires --subject and lists every validity interval; use --known-at to choose the recorded time")
		}
		db, err := readMemoryDB()
		if err != nil {
			return err
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
		defer cancel()
		var result store.FactResults
		if history {
			result, err = store.FactTimeline(ctx, db, store.TimelineQuery{Scope: *scope, Subject: subject, Predicate: predicate, KnownAt: known, Limit: limit})
		} else {
			result, err = store.QueryFacts(ctx, db, store.FactQuery{Scope: *scope, Subject: subject, Predicate: predicate, At: validAt, KnownAt: known, Limit: limit, IncludeStale: includeStale})
		}
		if err != nil {
			return err
		}
		for i := range result.Facts {
			result.Facts[i] = readFact(result.Facts[i])
		}
		if asJSON {
			out := map[string]any{"result": result, "known_at": known, "scope": *scope, "history": history, "local_only": true, "source_verified": false}
			if !history {
				out["at"] = validAt
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
		}
		return writeFactsText(cmd.OutOrStdout(), result)
	}
	parent.Flags().StringVar(&subject, "subject", "", "exact subject filter")
	parent.Flags().StringVar(&predicate, "predicate", "", "exact predicate filter")
	parent.Flags().StringVar(&at, "at", "", "claims valid at this RFC3339 time (default now)")
	parent.Flags().StringVar(&knownAt, "known-at", "", "claims recorded and not retracted at this RFC3339 time (default now)")
	parent.Flags().IntVar(&limit, "limit", 20, "maximum claims (1–100)")
	parent.Flags().BoolVar(&history, "history", false, "list every claim about --subject, including superseded values")
	parent.Flags().BoolVar(&includeStale, "include-stale", false, "include claims whose evidence no longer contains their quote or hash")
	parent.Flags().BoolVar(&asJSON, "json", false, "print machine-readable JSON")
}

// factsExtractCmd runs extraction explicitly: the deterministic rules by
// default, or the opt-in local Ollama extractor with --model.
func factsExtractCmd() *cobra.Command {
	var model, endpoint string
	var limit int
	var asJSON bool
	cmd := &cobra.Command{Use: "extract", Args: cobra.NoArgs, Short: "Derive evidence-linked claims from memories (rules by default; --model uses local Ollama)", RunE: func(cmd *cobra.Command, _ []string) error {
		if model == "" {
			if cmd.Flags().Changed("endpoint") {
				return fmt.Errorf("--endpoint requires --model")
			}
			if limit < 1 || limit > 10000 {
				return fmt.Errorf("limit must be 1–10000")
			}
			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			report, err := store.ExtractRules(ctx, db, store.RulesOptions{Limit: limit, At: time.Now().UnixMilli()})
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"protocol": 1, "facts": report, "local_only": true})
			}
			return writeExtractionText(cmd.OutOrStdout(), factsSummary{ExtractionReport: report})
		}
		report, err := learnedExtraction(cmd.Context(), model, endpoint, limit)
		if err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(report)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "facts (%s): %d added, %d retracted, %d rejected, %d conflicts skipped from %d memories; %d oversized and %d invalid outputs skipped (%d pending)\n",
			model, report.Facts.Added, report.Facts.Retracted, report.Facts.Rejected, report.Facts.Conflicts, report.Facts.Memories, report.SkippedOversized, report.InvalidOutput, report.Facts.Pending)
		return err
	}}
	cmd.Flags().StringVar(&model, "model", "", "exact installed Ollama model for opt-in learned extraction")
	cmd.Flags().StringVar(&endpoint, "endpoint", "http://127.0.0.1:11434", "loopback Ollama origin; remote endpoints and redirects are refused")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum memories (rules 1–10000, default 1000; learned 1–100, default 20)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print machine-readable JSON")
	cmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
		if !cmd.Flags().Changed("limit") {
			limit = 1000
			if model != "" {
				limit = 20
			}
		}
		return nil
	}
	return cmd
}

// generatorFactory builds the opt-in local extractor; tests replace it.
var generatorFactory semantic.GeneratorFactory = func(endpoint string) (semantic.Generator, error) { return semantic.NewOllama(endpoint) }

type learnedExtractionReport struct {
	Protocol         int                    `json:"protocol"`
	Model            string                 `json:"model"`
	ModelDigest      string                 `json:"model_digest"`
	Facts            store.ExtractionReport `json:"facts"`
	SkippedOversized int                    `json:"skipped_oversized"`
	InvalidOutput    int                    `json:"invalid_output"`
	LocalOnly        bool                   `json:"local_only"`
}

// learnedExtraction uses the same bounds as embed: an exact installed model,
// a loopback endpoint, a pinned digest, bounded requests and a five-minute
// deadline, and no SQLite transaction while the model runs.
func learnedExtraction(parent context.Context, model, endpoint string, limit int) (learnedExtractionReport, error) {
	report := learnedExtractionReport{Protocol: 1, Model: model, LocalOnly: true}
	if err := semantic.ValidateModel(model); err != nil {
		return report, err
	}
	if limit < 1 || limit > 100 {
		return report, fmt.Errorf("learned extraction limit must be 1–100")
	}
	configured, err := semantic.NewOllama(endpoint)
	if err != nil {
		return report, err
	}
	expected := configured.Origin()
	configured.Close()
	provider, err := generatorFactory(endpoint)
	if err != nil {
		return report, err
	}
	defer provider.Close()
	if provider.Origin() != expected {
		return report, fmt.Errorf("learned extractor origin differs from the configured local endpoint")
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	digest, err := provider.Digest(ctx, model)
	if err != nil {
		return report, err
	}
	report.ModelDigest = digest
	version := model + "@" + digest
	db, err := openDB()
	if err != nil {
		return report, err
	}
	defer db.Close()
	report.Facts = store.ExtractionReport{Extractor: store.ExtractorOllama, Version: version}
	memories, err := store.PendingExtraction(ctx, db, store.ExtractorOllama, version, limit, 0)
	if err != nil {
		return report, err
	}
	commit := func(apply func(*sql.Tx) error) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := apply(tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	for _, memory := range memories {
		at := time.Now().UnixMilli()
		if memory.Oversized || len(memory.Content) > extract.LearnedInputBytes {
			report.SkippedOversized++
			if err := commit(func(tx *sql.Tx) error {
				return store.RecordExtractionSkip(ctx, tx, memory, store.ExtractorOllama, version, at)
			}); err != nil {
				return report, err
			}
			continue
		}
		output, err := provider.Generate(ctx, model, extract.LearnedPrompt(memory.Content), extract.LearnedSchema)
		if err != nil {
			return report, fmt.Errorf("learned extraction stopped: %w", err)
		}
		if current, err := provider.Digest(ctx, model); err != nil || current != digest {
			return report, fmt.Errorf("model digest changed or became unavailable during extraction; rerun with a stable model")
		}
		claims, rejected, parseErr := extract.ParseLearned(memory.Content, output)
		err = commit(func(tx *sql.Tx) error {
			if parseErr != nil {
				report.InvalidOutput++
				return store.RecordExtractionSkip(ctx, tx, memory, store.ExtractorOllama, version, at)
			}
			result, err := store.ApplyExtraction(ctx, tx, memory, store.ExtractorOllama, version, claims, at)
			result.Rejected += rejected
			report.Facts.Add(result)
			return err
		})
		if err != nil {
			return report, err
		}
		if report.Facts.BudgetExhausted {
			break
		}
	}
	report.Facts.Pending, err = store.CountPendingExtraction(ctx, db, store.ExtractorOllama, version)
	return report, err
}
