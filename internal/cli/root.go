package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/spf13/cobra"
)

var dbPath string

func newRoot() *cobra.Command {
	return newRootWithModelProvider(localModelProvider)
}

func newRootWithModelProvider(factory modelFactory) *cobra.Command {
	root := &cobra.Command{Use: "trace-brain", Short: "Continuous memory over Trace history"}
	root.PersistentFlags().StringVar(&dbPath, "db", ".trace-brain/memory.db", "memory database path")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if !root.PersistentFlags().Changed("db") {
			if _, err := os.Stat(".trace-brain/memory.db"); os.IsNotExist(err) {
				if _, err := os.Stat(".trace/trace.db"); err == nil {
					dbPath = ".trace/brain/memory.db"
				}
			}
		}
		return nil
	}
	root.AddCommand(versionCmd())
	root.AddCommand(&cobra.Command{Use: "capabilities", Short: "Report retrieval modes and network policy without probing services", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"protocol": 1, "retrieval": map[string]any{"default": "sqlite_bm25", "legacy_semantic_flag": "hashed_words", "learned": "explicit_local_ollama", "model_service": "not_probed", "mcp_learned": "owner_opt_in_pinned_local_model", "mcp_learned_default": false}, "context_network": false, "memory_lifecycle": []string{"get", "update", "history", "forget", "verify_references"}, "workspace_retrieval": "explicit_read_only_databases", "documents": map[string]any{"sources": "explicit_local_text", "remote_connectors": false, "hosted_publication": false}, "facts": map[string]any{"input": "explicit_evidence_linked_claims", "temporal": "validity_and_recorded_time", "evidence": "current_or_captured_hash", "captured_evidence_budget_bytes": 33554432, "automatic_extraction": false, "source_verified": false}})
	}})
	root.AddCommand(statusCmd())
	root.AddCommand(rememberCmd())
	root.AddCommand(getCmd())
	root.AddCommand(updateCmd())
	root.AddCommand(forgetCmd())
	root.AddCommand(historyCmd())
	root.AddCommand(verifyCmd())
	root.AddCommand(workspaceCmd())
	root.AddCommand(documentsCmd())
	root.AddCommand(factsCmd())
	root.AddCommand(embedCmd(factory))
	root.AddCommand(semanticCmd(factory))
	root.AddCommand(recallCmd())
	root.AddCommand(queryCmd())
	root.AddCommand(overviewCmd())
	root.AddCommand(briefCmd())
	root.AddCommand(doctorCmd())
	root.AddCommand(stopCmd())
	root.AddCommand(extractCmd())
	root.AddCommand(watchCmd())
	root.AddCommand(vizCmd())
	root.AddCommand(dashCmd())
	root.AddCommand(setupCmd())
	root.AddCommand(mcpCmd(factory))
	root.AddCommand(contextCmd())
	root.AddCommand(refreshCmd())
	return root
}

func Execute() error {
	return newRoot().Execute()
}

func openDB() (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return nil, err
	}
	return store.Open(dbPath)
}

// Version is set by the coordinated Trace bundle build.
var Version = "dev"

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), "trace-brain "+Version)
	}}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{Use: "status", RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "memories: %d\n", n)
		return nil
	}}
}

func rememberCmd() *cobra.Command {
	var scope string
	cmd := &cobra.Command{Use: "remember <content>", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		content := strings.Join(args, " ")
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		id := fmt.Sprintf("mem-%d", time.Now().UnixNano())
		if scope == "" {
			scope = "project"
		}
		if err := store.Remember(cmd.Context(), db, id, scope, content, "manual", time.Now().UnixMilli()); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "remembered %s\n", id)
		return nil
	}}
	cmd.Flags().StringVar(&scope, "scope", "", "memory scope path")
	return cmd
}

func recallCmd() *cobra.Command {
	return &cobra.Command{Use: "recall <query>", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		q, err := store.LiteralQuery(strings.Join(args, " "))
		if err != nil {
			return err
		}
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		rows, err := db.QueryContext(cmd.Context(), `SELECT m.id,m.scope,substr(m.content,1,2048),m.created_at FROM memories m
			JOIN memory_fts f ON f.rowid=m.rowid WHERE memory_fts MATCH ? ORDER BY bm25(memory_fts),m.created_at DESC,m.id LIMIT 20`, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, scope, content string
			var created int64
			if err := rows.Scan(&id, &scope, &content, &created); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "[%s] %s: %s\n", id, scope, content)
		}
		return rows.Err()
	}}
}

func queryCmd() *cobra.Command {
	cmd := recallCmd()
	cmd.Use = "query <query>"
	return cmd
}

func overviewCmd() *cobra.Command {
	return &cobra.Command{Use: "overview", RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		fmt.Fprintln(cmd.OutOrStdout(), "Trace Brain overview")
		var n int
		db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n)
		fmt.Fprintf(cmd.OutOrStdout(), "  memories: %d\n", n)
		return nil
	}}
}

func briefCmd() *cobra.Command {
	return &cobra.Command{Use: "brief <task>", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		q, err := store.LiteralQuery(strings.Join(args, " "))
		if err != nil {
			return err
		}
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		rows, err := db.QueryContext(cmd.Context(), `SELECT substr(m.content,1,2048) FROM memories m JOIN memory_fts f ON f.rowid=m.rowid WHERE memory_fts MATCH ? ORDER BY bm25(memory_fts),m.created_at DESC,m.id LIMIT 10`, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		fmt.Fprintf(cmd.OutOrStdout(), "Brief for: %s\n", q)
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "- %s\n", c)
			}
		}
		return rows.Err()
	}}
}

func setupCmd() *cobra.Command {
	return &cobra.Command{Use: "setup [trace.db]", Args: cobra.MaximumNArgs(1), Short: "Import memories from a Trace repository", RunE: func(cmd *cobra.Command, args []string) error {
		traceDB := ".trace/trace.db"
		if len(args) > 0 {
			traceDB = args[0]
		}
		return importTrace(cmd.Context(), cmd.OutOrStdout(), traceDB, "", false)
	}}
}

func dashCmd() *cobra.Command {
	return &cobra.Command{Use: "dash", RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		fmt.Fprintln(cmd.OutOrStdout(), "=== trace-brain dashboard ===")
		var n int
		db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n)
		fmt.Fprintf(cmd.OutOrStdout(), "memories: %d\n", n)
		rows, _ := db.Query("SELECT id, scope, content FROM memories ORDER BY created_at DESC LIMIT 10")
		if rows != nil {
			defer rows.Close()
			for rows.Next() {
				var id, scope, content string
				if err := rows.Scan(&id, &scope, &content); err == nil {
					fmt.Fprintf(cmd.OutOrStdout(), "  [%s] %s: %s\n", id, scope, content)
				}
			}
		}
		return nil
	}}
}

func vizCmd() *cobra.Command {
	return &cobra.Command{Use: "viz", RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		path := filepath.Join(filepath.Dir(dbPath), "viz.html")
		if err := renderVisualization(db, path); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)
		return err
	}}
}

func extractCmd() *cobra.Command {
	var agentCmd, traceDB string
	cmd := &cobra.Command{Use: "extract", Short: "Extract memories from Trace sessions using an agent", RunE: func(cmd *cobra.Command, args []string) error {
		return importTrace(cmd.Context(), cmd.OutOrStdout(), traceDB, agentCmd, true)
	}}
	cmd.Flags().StringVar(&agentCmd, "agent", "", "agent command to run (reads prompt on stdin, writes memory on stdout)")
	cmd.Flags().StringVar(&traceDB, "trace-db", ".trace/trace.db", "path to Trace database")
	return cmd
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{Use: "doctor", RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return fmt.Errorf("database: %w", err)
		}
		defer db.Close()
		fmt.Fprintln(cmd.OutOrStdout(), "trace-brain doctor: ok")
		return nil
	}}
}
