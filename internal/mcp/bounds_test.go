package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-brain/internal/store"
)

func TestMemoryBoundsAndLiteralQueries(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db)
	for i := 0; i < 55; i++ {
		if _, _, err = s.remember(context.Background(), nil, rememberInput{Content: "auth-token " + strings.Repeat("x", 3000)}); err != nil {
			t.Fatal(err)
		}
	}
	_, out, err := s.recall(context.Background(), nil, recallInput{Query: "auth-token", Limit: 999999})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Memories) > 50 || len(out.Memories) == 0 {
		t.Fatalf("unbounded or missing results: %d", len(out.Memories))
	}
	for _, memory := range out.Memories {
		if len(memory["content"].(string)) > 2048 {
			t.Fatal("unbounded content")
		}
	}
	var n int
	if err = db.QueryRow("SELECT count(*) FROM embeddings").Scan(&n); err != nil || n != 55 {
		t.Fatalf("MCP memory embeddings: %d %v", n, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err = s.recall(ctx, nil, recallInput{Query: "auth"}); err == nil {
		t.Fatal("cancelled query succeeded")
	}
}

func TestMemoryHistoryContentIsBoundedAndMarked(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := store.Remember(ctx, db, "m", "project", strings.Repeat("中", 3000), "mcp", 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(ctx, db, "m", "", strings.Repeat("文", 3000), 2); err != nil {
		t.Fatal(err)
	}
	_, history, err := New(db).history(ctx, nil, memoryID{ID: "m"})
	if err != nil || len(history.Revisions) != 2 {
		t.Fatalf("history: %+v %v", history, err)
	}
	for _, version := range history.Revisions {
		if len([]rune(version.Content)) != 2048 || !version.ContentTruncated {
			t.Fatalf("unbounded history version: %d", len([]rune(version.Content)))
		}
	}
}
