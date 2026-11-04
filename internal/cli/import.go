package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/embed"
	"github.com/UseTrace-SI/trace-brain/internal/redact"
)

func importTrace(ctx context.Context, out io.Writer, path, agentCmd string, extract bool) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("opening Trace source: %w", err)
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	src, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer src.Close()
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	var tx *sql.Tx
	if !extract {
		tx, err = db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}
	if !extract {
		if _, err := tx.ExecContext(ctx, "CREATE TEMP TABLE IF NOT EXISTS import_seen(kind TEXT,id TEXT,PRIMARY KEY(kind,id)); DELETE FROM import_seen"); err != nil {
			return err
		}
	}
	source := "trace-import"
	if extract {
		source = "agent-extract"
	}
	save := func(kind, entityID, sid, tid, content string) (bool, error) {
		if !extract {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO import_seen VALUES(?,?)", kind, entityID); err != nil {
				return false, err
			}
		}
		content = (redact.Redactor{}).Text(content)
		fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(content+"\x00"+agentCmd)))
		var old string
		var err error
		if extract {
			err = db.QueryRowContext(ctx, "SELECT fingerprint FROM memory_imports WHERE source_db=? AND source=? AND entity_kind=? AND entity_id=?", path, source, kind, entityID).Scan(&old)
		} else {
			err = tx.QueryRowContext(ctx, "SELECT fingerprint FROM memory_imports WHERE source_db=? AND source=? AND entity_kind=? AND entity_id=?", path, source, kind, entityID).Scan(&old)
		}
		if err != nil && err != sql.ErrNoRows {
			return false, err
		}
		if old == fingerprint {
			return false, nil
		}
		if extract && agentCmd != "" {
			agentCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			c := exec.CommandContext(agentCtx, "sh", "-c", agentCmd)
			// Cancel the entire command group, including an agent launched by
			// the shell. Killing only the shell can leave its children holding
			// stdout open and prevent watcher shutdown indefinitely.
			c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			c.Cancel = func() error {
				err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
				if err == syscall.ESRCH {
					return os.ErrProcessDone
				}
				return err
			}
			c.WaitDelay = time.Second
			c.Stdin = strings.NewReader("Extract a concise memory (one sentence) about this session: " + content)
			output := &extractBuffer{limit: 65536, cancel: cancel}
			stderr := &extractBuffer{limit: 4096, cancel: cancel}
			c.Stdout = output
			c.Stderr = stderr
			err := c.Run()
			if output.exceeded || stderr.exceeded {
				return false, fmt.Errorf("extracting session %s: output exceeds limit", sid)
			}
			if err != nil {
				return false, fmt.Errorf("extracting session %s: %w", sid, err)
			}
			content = (redact.Redactor{}).Text(strings.TrimSpace(output.String()))
			if content == "" {
				return false, fmt.Errorf("extracting session %s: empty response", sid)
			}
		}
		writeTx := tx
		if extract {
			writeTx, err = db.BeginTx(ctx, nil)
			if err != nil {
				return false, err
			}
			defer writeTx.Rollback()
		}
		id := fmt.Sprintf("import-%x", sha256.Sum256([]byte(path+"\x00"+source+"\x00"+kind+"\x00"+entityID)))
		if _, err := writeTx.ExecContext(ctx, `INSERT INTO memories(id,scope,content,source,session_id,turn_id,created_at) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET content=excluded.content`, id, kind, content, source, sid, tid, time.Now().UnixMilli()); err != nil {
			return false, err
		}
		_, err = writeTx.ExecContext(ctx, `INSERT INTO memory_imports(source_db,source,entity_kind,entity_id,fingerprint,memory_id) VALUES(?,?,?,?,?,?)
ON CONFLICT(source_db,source,entity_kind,entity_id) DO UPDATE SET fingerprint=excluded.fingerprint,memory_id=excluded.memory_id`, path, source, kind, entityID, fingerprint, id)
		if err != nil {
			return false, err
		}
		_, err = writeTx.ExecContext(ctx, `INSERT OR REPLACE INTO embeddings(memory_id,dim,vector) VALUES(?,?,?)`,
			id, embed.Dim, embed.Encode(embed.Embed(content)))
		if err != nil {
			return false, err
		}
		if extract {
			if err = writeTx.Commit(); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	rows, err := src.QueryContext(ctx, "SELECT id, agent, worktree, started_at FROM sessions")
	if err != nil {
		return err
	}
	sessions := 0
	for rows.Next() {
		var sid, agent, worktree string
		var started int64
		if err := rows.Scan(&sid, &agent, &worktree, &started); err != nil {
			rows.Close()
			return err
		}
		content := fmt.Sprintf("Session %s: agent=%s worktree=%s started=%d", sid, agent, worktree, started)
		if extract {
			evidence, err := sessionEvidence(ctx, src, sid, agent, started)
			if err != nil {
				rows.Close()
				return err
			}
			content = evidence
		}
		changed, err := save("sessions", sid, sid, "", content)
		if err != nil {
			rows.Close()
			return err
		}
		if changed {
			sessions++
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	turns := 0
	if !extract {
		rows, err := src.QueryContext(ctx, "SELECT id, session_id, seq, prompt, reasoning FROM turns")
		if err != nil {
			return err
		}
		for rows.Next() {
			var tid, sid string
			var seq int
			var prompt, reasoning sql.NullString
			if err := rows.Scan(&tid, &sid, &seq, &prompt, &reasoning); err != nil {
				rows.Close()
				return err
			}
			content := fmt.Sprintf("Turn %s (session %s, seq %d): %s", tid, sid, seq, prompt.String)
			if reasoning.String != "" {
				content += " | reasoning: " + reasoning.String
			}
			changed, err := save("turns", tid, sid, tid, content)
			if err != nil {
				rows.Close()
				return err
			}
			if changed {
				turns++
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	if !extract {
		if _, err := tx.ExecContext(ctx, `DELETE FROM memories WHERE id IN (SELECT i.memory_id FROM memory_imports i WHERE i.source_db=? AND i.source='trace-import' AND NOT EXISTS(SELECT 1 FROM import_seen s WHERE s.kind=i.entity_kind AND s.id=i.entity_id))`, path); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM memory_imports WHERE source_db=? AND source='trace-import' AND NOT EXISTS(SELECT 1 FROM memories m WHERE m.id=memory_imports.memory_id)", path); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if extract {
		_, err = fmt.Fprintf(out, "extracted %d memories\n", sessions)
	} else {
		_, err = fmt.Fprintf(out, "imported %d sessions\nimported %d turns\n", sessions, turns)
	}
	return err
}

// Stop a noisy subprocess before it can exhaust the watcher process.
type extractBuffer struct {
	bytes.Buffer
	limit    int
	cancel   context.CancelFunc
	exceeded bool
}

func (b *extractBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		b.exceeded = true
		b.cancel()
		return 0, fmt.Errorf("extraction output limit exceeded")
	}
	return b.Buffer.Write(p)
}
