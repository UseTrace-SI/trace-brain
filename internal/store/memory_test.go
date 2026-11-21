package store

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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

// Oversized or malformed Trace history used to be rejected, which failed the
// whole import and every later refresh of that history. It must instead be
// bounded explicitly: redaction runs before any cut, the prompt (head) and the
// latest outcome (tail) survive, and no cut splits UTF-8 or a redaction marker.
func TestRegressionBoundImportedContentKeepsHeadTailAndRedactsBeforeCutting(t *testing.T) {
	r := redact.Redactor{}
	token := "ghp_" + strings.Repeat("Zx9", 12)
	nearCut := strings.Repeat("é", 24000) + " " + token + " " + strings.Repeat("ü", 30000) + " FINAL-OUTCOME"
	markers := strings.Repeat(token+" ", 3000) + "END"
	for _, tc := range []struct {
		name, input    string
		wantBounded    bool
		mustContain    []string
		mustNotContain []string
	}{
		{"small text is only redacted", "Turn t: deploy with " + token, false, []string{"Turn t: deploy with ⟨redacted:github-token:"}, []string{token}},
		{"invalid UTF-8 is normalized", "bad \xff\xfe bytes", false, []string{"bad \uFFFD bytes"}, nil},
		{"secret near the cut is redacted before cutting", "PROMPT-HEAD " + nearCut, true, []string{"PROMPT-HEAD", "FINAL-OUTCOME", "bytes omitted to fit the 64 KiB memory budget"}, []string{token, "ghp_Zx9"}},
		{"markers are never split", markers, true, []string{"END"}, []string{"ghp_"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BoundImportedContent(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) > maxMemoryContentBytes || !utf8.ValidString(got) {
				t.Fatalf("unbounded or invalid result: %d bytes, valid=%v", len(got), utf8.ValidString(got))
			}
			if bounded := strings.Contains(got, "bytes omitted"); bounded != tc.wantBounded {
				t.Fatalf("omission notice present=%v, want %v", bounded, tc.wantBounded)
			}
			for _, want := range tc.mustContain {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q", want)
				}
			}
			for _, unwanted := range tc.mustNotContain {
				if strings.Contains(got, unwanted) {
					t.Fatalf("leaked %q", unwanted)
				}
			}
			if r.Text(got) != got {
				t.Fatal("bounded text is not a redaction fixed point")
			}
			for _, fragment := range regexp.MustCompile(`⟨[^⟩]*$|^[^⟨]*⟩`).FindAllString(strings.ReplaceAll(got, "\n[trace-brain:", "⟦"), -1) {
				t.Fatalf("split redaction marker: %q", fragment)
			}
		})
	}
	if _, err := BoundImportedContent(" \n\t"); err == nil {
		t.Fatal("accepted empty imported text")
	}
}

// Bounding redacted the whole raw turn on every refresh, so one huge turn with
// many secrets made each refresh exceed Trace's one-minute limit. Only bounded
// windows of each end are redacted now, far larger than the kept text.
func TestBoundImportedContentRedactsOnlyBoundedWindowsOfHugeTurns(t *testing.T) {
	secret := "api value 7dF2aQ9mR4xT8vL1pN6cY3zB5kH0sW2u\n"
	huge := "PROMPT-HEAD\n" + strings.Repeat(secret, (20<<20)/len(secret)) + "FINAL-OUTCOME"
	start := time.Now()
	got, err := BoundImportedContent(huge)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("bounding a 20 MiB turn took %s", elapsed)
	}
	if len(got) > maxMemoryContentBytes || !strings.HasPrefix(got, "PROMPT-HEAD") || !strings.HasSuffix(got, "FINAL-OUTCOME") || strings.Contains(got, "7dF2aQ9mR4xT8vL1pN6cY3zB5kH0sW2u") {
		t.Fatalf("bounded huge turn: %d bytes, head/tail kept=%v/%v", len(got), strings.HasPrefix(got, "PROMPT-HEAD"), strings.HasSuffix(got, "FINAL-OUTCOME"))
	}
	var omitted int
	if _, err := fmt.Sscanf(got[strings.Index(got, "[trace-brain: "):], "[trace-brain: %d bytes omitted", &omitted); err != nil || omitted < len(huge)-2*maxMemoryContentBytes {
		t.Fatalf("omission notice understates the dropped bytes: %d of %d (%v)", omitted, len(huge), err)
	}
	if (redact.Redactor{}).Text(got) != got {
		t.Fatal("bounded text is not a redaction fixed point")
	}
}
