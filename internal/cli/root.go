package cli

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/embed"
	"github.com/UseTrace-SI/trace-brain/internal/mcp"
	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/spf13/cobra"
)

var dbPath string

func newRoot() *cobra.Command {
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
	root.AddCommand(statusCmd())
	root.AddCommand(rememberCmd())
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
	root.AddCommand(mcpCmd())
	root.AddCommand(contextCmd())
	root.AddCommand(refreshCmd())
	return root
}

func Execute() error {
	return newRoot().Execute()
}

func openDB() (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, err
	}
	return store.Open(dbPath)
}

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), "trace-brain dev")
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
		now := time.Now().UnixMilli()
		_, err = db.Exec("INSERT INTO memories(id,scope,content,source,created_at) VALUES(?,?,?,?,?)",
			id, scope, content, "manual", now)
		if err != nil {
			return err
		}
		_, err = db.Exec("INSERT OR REPLACE INTO embeddings(memory_id,dim,vector) VALUES(?,?,?)",
			id, embed.Dim, embed.Encode(embed.Embed(content)))
		if err != nil {
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
		q := strings.Join(args, " ")
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		rows, err := db.Query(`SELECT m.id,m.scope,m.content,m.created_at FROM memories m
			JOIN memory_fts f ON f.rowid=m.rowid WHERE memory_fts MATCH ? ORDER BY m.created_at DESC LIMIT 20`, q)
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
		q := strings.Join(args, " ")
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		rows, err := db.Query(`SELECT m.content FROM memories m JOIN memory_fts f ON f.rowid=m.rowid WHERE memory_fts MATCH ? LIMIT 10`, q)
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

func mcpCmd() *cobra.Command {
	return &cobra.Command{Use: "mcp", Short: "Serve memory tools over MCP stdio", RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		return mcp.New(db).Run(cmd.Context())
	}}
}
