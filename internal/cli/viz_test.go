package cli

import (
	"context"
	"fmt"
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
	for i := 0; i < vizLimit+5; i++ {
		if _, err := db.Exec("INSERT INTO memories(id,scope,content,created_at) VALUES(?,?,?,?)", fmt.Sprintf("m%04d", i), "project", "content", i); err != nil {
			t.Fatal(err)
		}
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
