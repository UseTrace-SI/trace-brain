package store

import (
	"context"
	"testing"
)

func TestLiteralQueryAndAtomicRemember(t *testing.T) {
	db, err := Open(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = Remember(context.Background(), db, "m", "project", "fix auth-token refresh 中文", "manual", 1); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"auth-token", `auth "token"`, "中文", "", "!!!"} {
		q, err := LiteralQuery(text)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err = db.QueryRow("SELECT count(*) FROM memory_fts WHERE memory_fts MATCH ?", q).Scan(&n); err != nil {
			t.Fatalf("query %q: %v", text, err)
		}
	}
	if _, err = db.Exec("CREATE TRIGGER reject_vector BEFORE INSERT ON embeddings BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if err = Remember(context.Background(), db, "bad", "project", "must roll back", "mcp", 2); err == nil {
		t.Fatal("embedding failure accepted")
	}
	var n int
	if err = db.QueryRow("SELECT count(*) FROM memories WHERE id='bad'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial memory persisted: %d %v", n, err)
	}
}
