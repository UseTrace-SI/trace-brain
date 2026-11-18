package store

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/UseTrace-SI/trace-brain/internal/redact"
)

func TestRegressionMemoryIDsAreRandomAndUnique(t *testing.T) {
	db, err := Open(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pattern := regexp.MustCompile(`^mem-[a-f0-9]{32}$`)
	seen := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := NewMemoryID(context.Background(), db)
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if !pattern.MatchString(id) {
				errs <- fmt.Errorf("unexpected memory ID shape: %q", id)
			}
			if seen[id] {
				errs <- fmt.Errorf("duplicate memory ID: %q", id)
			}
			seen[id] = true
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

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

// Redaction markers expand the serialized bytes. A raw payload that fits the
// budget must still be rejected once redaction pushes it past the limit, or
// readers (substr), fact evidence and semantic caches see truncated text.
func TestRegressionRememberRejectsOversizeAfterRedaction(t *testing.T) {
	db, err := Open(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := redact.Redactor{}
	secret := "ghp_abcdefghijklmnopqrst" // 24 bytes, expands to a 36-byte marker
	content := strings.Repeat("a", 65536-len(secret)-1) + " " + secret
	scope := strings.Repeat("s", 1024-len(secret)-1) + " " + secret
	if len(content) != 65536 || len(scope) != 1024 || len(r.Text(content)) <= 65536 || len(r.Text(scope)) <= 1024 {
		t.Fatalf("invalid fixture: content=%d/%d scope=%d/%d", len(content), len(r.Text(content)), len(scope), len(r.Text(scope)))
	}
	if err := Remember(context.Background(), db, "oversize-content", "project", content, "manual", 1); err == nil {
		t.Fatal("accepted content that exceeds the budget after redaction")
	}
	if err := Remember(context.Background(), db, "oversize-scope", scope, "content", "manual", 2); err == nil {
		t.Fatal("accepted scope that exceeds the budget after redaction")
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM memories").Scan(&n); err != nil || n != 0 {
		t.Fatalf("oversized memory persisted: %d %v", n, err)
	}
}
