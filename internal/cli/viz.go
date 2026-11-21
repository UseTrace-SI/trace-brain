package cli

import (
	"context"
	"database/sql"
	"html/template"
	"os"
	"path/filepath"

	"github.com/UseTrace-SI/trace-brain/internal/redact"
)

// vizLimit bounds both the SQL scan and the rendered document. The cap and its
// truncation flag are explicit so a large database cannot produce an unbounded
// page without the caller knowing.
const vizLimit = 1000

var visualization = template.Must(template.New("viz").Parse(`<!doctype html><html><head><meta charset="utf-8"><title>Trace Brain</title><style>body{font-family:sans-serif;margin:20px}.m{border:1px solid #ccc;padding:10px;margin:10px 0;border-radius:8px}.scope{color:#666;font-size:12px}.truncated{color:#a00}</style></head><body><h1>Trace Brain</h1>{{if .Truncated}}<p class="truncated">Showing only the {{.Limit}} most recent memories (truncated).</p>{{end}}{{range .Memories}}<div class="m"><div class="scope">{{.Scope}} | {{.ID}}</div><div>{{.Content}}</div></div>{{end}}</body></html>`))

func renderVisualization(ctx context.Context, db *sql.DB, path string) (bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT id,scope,substr(content,1,65536) FROM memories ORDER BY created_at DESC,id DESC LIMIT ?", vizLimit+1)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	type memory struct{ ID, Scope, Content string }
	memories := make([]memory, 0, vizLimit+1)
	for rows.Next() {
		var m memory
		if err := rows.Scan(&m.ID, &m.Scope, &m.Content); err != nil {
			return false, err
		}
		m.Scope, m.Content = (redact.Redactor{}).Text(m.Scope), (redact.Redactor{}).Text(m.Content)
		memories = append(memories, m)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	truncated := len(memories) > vizLimit
	if truncated {
		memories = memories[:vizLimit]
	}
	data := struct {
		Memories  []memory
		Truncated bool
		Limit     int
	}{Memories: memories, Truncated: truncated, Limit: vizLimit}
	f, err := os.CreateTemp(filepath.Dir(path), ".viz-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name())
	if err := visualization.Execute(f, data); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return false, err
	}
	return truncated, nil
}
