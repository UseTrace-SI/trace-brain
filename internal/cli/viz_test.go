package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-brain/internal/store"
)

func TestRegressionVisualizationCapsRowsAndReportsTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Build the fixture with one SQL statement. Repeated per-row Exec calls
	// through the pure-Go SQLite driver dominate this test under -race when CI
	// repeats the regression suite 100 times.
	if _, err := db.Exec(`WITH RECURSIVE seq(i) AS (
		SELECT 0 UNION ALL SELECT i+1 FROM seq WHERE i < ?
	)
	INSERT INTO memories(id,scope,content,created_at)
	SELECT printf('m%04d', i), 'project', 'content', i FROM seq`, vizLimit+4); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "viz.html")
	truncated, err := renderVisualization(context.Background(), db, out)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("truncation was not reported")
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), `class="m"`); n != vizLimit {
		t.Fatalf("rendered %d memories, want %d", n, vizLimit)
	}
	if !strings.Contains(string(b), "truncated") {
		t.Fatal("truncation was not reported in the output")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := renderVisualization(ctx, db, out); err == nil {
		t.Fatal("cancelled visualization succeeded")
	}
}
