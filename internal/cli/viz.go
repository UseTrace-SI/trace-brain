package cli

import (
	"database/sql"
	"html/template"
	"os"
	"path/filepath"
)

var visualization = template.Must(template.New("viz").Parse(`<!doctype html><html><head><meta charset="utf-8"><title>Trace Brain</title><style>body{font-family:sans-serif;margin:20px}.m{border:1px solid #ccc;padding:10px;margin:10px 0;border-radius:8px}.scope{color:#666;font-size:12px}</style></head><body><h1>Trace Brain</h1>{{range .}}<div class="m"><div class="scope">{{.Scope}} | {{.ID}}</div><div>{{.Content}}</div></div>{{end}}</body></html>`))

func renderVisualization(db *sql.DB, path string) error {
	rows, err := db.Query("SELECT id,scope,content FROM memories ORDER BY created_at DESC")
	if err != nil {
		return err
	}
	defer rows.Close()
	type memory struct{ ID, Scope, Content string }
	var memories []memory
	for rows.Next() {
		var m memory
		if err := rows.Scan(&m.ID, &m.Scope, &m.Content); err != nil {
			return err
		}
		memories = append(memories, m)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".viz-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := visualization.Execute(f, memories); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
