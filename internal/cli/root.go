package cli

import (
	"os/exec"
	"syscall"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/mcp"
	"github.com/UseTrace-SI/trace-brain/internal/store"
	"github.com/spf13/cobra"
)

var dbPath string

func newRoot() *cobra.Command {
	root := &cobra.Command{Use: "trace-brain", Short: "Continuous memory over Trace history"}
	root.PersistentFlags().StringVar(&dbPath, "db", ".trace-brain/memory.db", "memory database path")
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
		fmt.Println("trace-brain dev")
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
		fmt.Printf("memories: %d\n", n)
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
		_, err = db.Exec("INSERT INTO memories(id,scope,content,source,created_at) VALUES(?,?,?,?,?)",
			id, scope, content, "manual", time.Now().UnixMilli())
		if err != nil {
			return err
		}
		fmt.Printf("remembered %s\n", id)
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
			fmt.Printf("[%s] %s: %s\n", id, scope, content)
		}
		return rows.Err()
	}}
}

func queryCmd() *cobra.Command {
	return recallCmd()
}

func overviewCmd() *cobra.Command {
	return &cobra.Command{Use: "overview", RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		fmt.Println("Trace Brain overview")
		var n int
		db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n)
		fmt.Printf("  memories: %d\n", n)
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
		fmt.Printf("Brief for: %s\n", q)
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err == nil {
				fmt.Printf("- %s\n", c)
			}
		}
		return rows.Err()
	}}
}

func setupCmd() *cobra.Command {
	return &cobra.Command{Use: "setup", Short: "Import memories from a Trace repository", RunE: func(cmd *cobra.Command, args []string) error {
		traceDB := ".trace/trace.db"
		if len(args) > 0 {
			traceDB = args[0]
		}
		src, err := sql.Open("sqlite", traceDB)
		if err != nil {
			return err
		}
		defer src.Close()
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		rows, err := src.Query("SELECT id, agent, worktree, started_at FROM sessions")
		if err != nil {
			return err
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var sid, agent, worktree string
			var started int64
			if err := rows.Scan(&sid, &agent, &worktree, &started); err != nil {
				return err
			}
			content := fmt.Sprintf("Session %s: agent=%s worktree=%s started=%d", sid, agent, worktree, started)
			id := fmt.Sprintf("mem-%d", time.Now().UnixNano())
			if _, err := db.Exec("INSERT OR IGNORE INTO memories(id,scope,content,source,session_id,created_at) VALUES(?,?,?,?,?,?)",
				id, "sessions", content, "trace-import", sid, time.Now().UnixMilli()); err != nil {
				return err
			}
			n++
		}
		fmt.Printf("imported %d sessions\n", n)
		// import turns
		trows, err := src.Query("SELECT id, session_id, seq, prompt, reasoning, started_at FROM turns")
		if err == nil {
			defer trows.Close()
			tn := 0
			for trows.Next() {
				var tid, sid string
				var seq int
				var prompt, reasoning *string
				var started int64
				if err := trows.Scan(&tid, &sid, &seq, &prompt, &reasoning, &started); err != nil {
					continue
				}
				content := fmt.Sprintf("Turn %s (session %s, seq %d): %s", tid, sid, seq, deref(prompt))
				if reasoning != nil && *reasoning != "" {
					content += " | reasoning: " + *reasoning
				}
				memID := fmt.Sprintf("mem-%d", time.Now().UnixNano())
				db.Exec("INSERT OR IGNORE INTO memories(id,scope,content,source,session_id,turn_id,created_at) VALUES(?,?,?,?,?,?,?)",
					memID, "turns", content, "trace-import", sid, tid, time.Now().UnixMilli())
				tn++
			}
			fmt.Printf("imported %d turns\n", tn)
		}
		return rows.Err()
	}}
}

func dashCmd() *cobra.Command {
	return &cobra.Command{Use: "dash", RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		fmt.Println("=== trace-brain dashboard ===")
		var n int
		db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n)
		fmt.Printf("memories: %d\n", n)
		rows, _ := db.Query("SELECT id, scope, content FROM memories ORDER BY created_at DESC LIMIT 10")
		if rows != nil {
			defer rows.Close()
			for rows.Next() {
				var id, scope, content string
				if err := rows.Scan(&id, &scope, &content); err == nil {
					fmt.Printf("  [%s] %s: %s\n", id, scope, content)
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
		rows, err := db.Query("SELECT id, scope, content, created_at FROM memories ORDER BY created_at DESC")
		if err != nil {
			return err
		}
		defer rows.Close()
		var html string
		html = "<html><head><title>Trace Brain</title><style>body{font-family:sans-serif;margin:20px;} .m{border:1px solid #ccc;padding:10px;margin:10px 0;border-radius:8px;} .scope{color:#666;font-size:12px;}</style></head><body><h1>Trace Brain</h1>"
		for rows.Next() {
			var id, scope, content string
			var created int64
			if err := rows.Scan(&id, &scope, &content, &created); err != nil {
				continue
			}
			html += "<div class='m'><div class='scope'>" + scope + " | " + id + "</div><div>" + content + "</div></div>"
		}
		html += "</body></html>"
		path := ".trace-brain/viz.html"
		if err := os.MkdirAll(".trace-brain", 0755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(html), 0644); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", path)
		return nil
	}}
}


func extractCmd() *cobra.Command {
	var agentCmd string
	var traceDB string
	cmd := &cobra.Command{Use: "extract", Short: "Extract memories from Trace sessions using an agent", RunE: func(cmd *cobra.Command, args []string) error {
		if agentCmd == "" {
			// rule-based extraction fallback
			agentCmd = ""
		}
		src, err := sql.Open("sqlite", traceDB)
		if err != nil {
			return err
		}
		defer src.Close()
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		rows, err := src.Query("SELECT id, agent, worktree, started_at FROM sessions")
		if err != nil {
			return err
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var sid, agent, worktree string
			var started int64
			if err := rows.Scan(&sid, &agent, &worktree, &started); err != nil {
				continue
			}
			var content string
			if agentCmd != "" {
				prompt := fmt.Sprintf("Extract a concise memory (one sentence) about this session: agent=%s worktree=%s started=%d", agent, worktree, started)
				c := exec.Command("sh", "-c", agentCmd)
				c.Stdin = strings.NewReader(prompt)
				out, err := c.Output()
				if err != nil {
					continue
				}
				content = strings.TrimSpace(string(out))
			} else {
				content = fmt.Sprintf("Session by %s in %s (started %d)", agent, worktree, started)
			}
			if content == "" {
				continue
			}
			memID := fmt.Sprintf("mem-%d", time.Now().UnixNano())
			db.Exec("INSERT OR IGNORE INTO memories(id,scope,content,source,session_id,created_at) VALUES(?,?,?,?,?,?)",
				memID, "sessions", content, "agent-extract", sid, time.Now().UnixMilli())
			n++
		}
		fmt.Printf("extracted %d memories\n", n)
		return rows.Err()
	}}
	cmd.Flags().StringVar(&agentCmd, "agent", "", "agent command to run (reads prompt on stdin, writes memory on stdout)")
	cmd.Flags().StringVar(&traceDB, "trace-db", ".trace/trace.db", "path to Trace database")
	return cmd
}

func watchCmd() *cobra.Command {
	var interval int
	var agentCmd string
	var traceDB string
	var daemon bool
	cmd := &cobra.Command{Use: "watch", Short: "Background watcher: periodically extract memories", RunE: func(cmd *cobra.Command, args []string) error {
		if daemon {
			// fork
			exe, _ := os.Executable()
			c := exec.Command(exe, "watch", "--interval", fmt.Sprintf("%d", interval), "--agent", agentCmd, "--trace-db", traceDB, "--db", dbPath)
			c.Stdin = nil
			logf, _ := os.OpenFile(dbPath+".watch.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
			c.Stdout = logf
			c.Stderr = logf
			c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := c.Start(); err != nil {
				return err
			}
			pidPath := dbPath + ".watch.pid"
			os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", c.Process.Pid)), 0644)
			c.Process.Release()
			fmt.Printf("watcher started (pid %d)\n", c.Process.Pid)
			return nil
		}
		if interval < 1 {
			interval = 60
		}
		fmt.Printf("watching every %ds (agent=%s)\n", interval, agentCmd)
		for {
			ec := exec.Command(os.Args[0], "extract", "--agent", agentCmd, "--trace-db", traceDB, "--db", dbPath)
			ec.Stdout = os.Stdout
			ec.Stderr = os.Stderr
			_ = ec.Run()
			time.Sleep(time.Duration(interval) * time.Second)
		}
	}}
	cmd.Flags().IntVar(&interval, "interval", 60, "seconds between runs")
	cmd.Flags().StringVar(&agentCmd, "agent", "", "agent command")
	cmd.Flags().StringVar(&traceDB, "trace-db", ".trace/trace.db", "path to Trace database")
	cmd.Flags().BoolVar(&daemon, "daemon", false, "run in background")
	return cmd
}

func stopCmd() *cobra.Command {
	return &cobra.Command{Use: "stop", Short: "Stop background watcher", RunE: func(cmd *cobra.Command, args []string) error {
		pidPath := dbPath + ".watch.pid"
		b, err := os.ReadFile(pidPath)
		if err != nil {
			return fmt.Errorf("no watcher pid file")
		}
		pid := 0
		fmt.Sscanf(string(b), "%d", &pid)
		if pid <= 0 {
			return fmt.Errorf("invalid pid")
		}
		p, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		if err := p.Kill(); err != nil {
			return err
		}
		os.Remove(pidPath)
		fmt.Printf("stopped watcher %d\n", pid)
		return nil
	}}
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{Use: "doctor", RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return fmt.Errorf("database: %w", err)
		}
		defer db.Close()
		fmt.Println("trace-brain doctor: ok")
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

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
