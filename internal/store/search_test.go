package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/redact"
)

func searchTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func insertMemory(t *testing.T, db *sql.DB, id, scope, content, source string, created int64) {
	t.Helper()
	if _, err := db.Exec("INSERT INTO memories(id,scope,content,source,created_at) VALUES(?,?,?,?,?)", id, scope, content, source, created); err != nil {
		t.Fatal(err)
	}
}

func resultIDs(results []SearchResult) []string {
	ids := make([]string, len(results))
	for i, r := range results {
		ids[i] = r.ID
	}
	return ids
}

var searchNow = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

func daysAgo(days int) int64 { return searchNow.Add(-time.Duration(days) * 24 * time.Hour).UnixMilli() }

func TestSearchBlendsRelevanceRecencyAndManualBoost(t *testing.T) {
	db := searchTestDB(t)
	// Equal text: recency decides; equal age: the manual note wins.
	insertMemory(t, db, "old", "project", "cache invalidation strategy", "trace-import", daysAgo(400))
	insertMemory(t, db, "new", "project", "cache invalidation strategy notes", "trace-import", daysAgo(1))
	insertMemory(t, db, "manual", "project", "cache invalidation strategy notes", "manual", daysAgo(1))
	results, err := Search(context.Background(), db, SearchOpts{Query: "cache invalidation", Now: searchNow})
	if err != nil {
		t.Fatal(err)
	}
	// "new" and "manual" normalize to the same text, so only the better
	// ranked (manual) copy remains.
	if got := strings.Join(resultIDs(results), ","); got != "manual,old" {
		t.Fatalf("ranking %s", got)
	}
	for i := 1; i < len(results); i++ {
		if results[i-1].Score < results[i].Score {
			t.Fatalf("scores not descending: %+v", results)
		}
	}
}

func TestSearchRelevanceOutweighsRecencyForStrongerMatches(t *testing.T) {
	db := searchTestDB(t)
	insertMemory(t, db, "specific", "project", "authentication timeout retry deadline authentication timeout", "trace-import", daysAgo(200))
	insertMemory(t, db, "weak", "project", "authentication general notes about many unrelated topics and other words", "trace-import", daysAgo(0))
	results, err := Search(context.Background(), db, SearchOpts{Query: "authentication timeout", Now: searchNow})
	if err != nil || len(results) != 2 || results[0].ID != "specific" {
		t.Fatalf("weak recent match outranked evidence: %v %v", resultIDs(results), err)
	}
}

func TestSearchCollapsesNearDuplicatesAndStillFillsLimit(t *testing.T) {
	db := searchTestDB(t)
	for i := 0; i < 10; i++ {
		// Case and whitespace differences are near-duplicates.
		insertMemory(t, db, fmt.Sprintf("dup-%02d", i), "project", fmt.Sprintf("Deploy%sthe %s", strings.Repeat(" ", i+1), strings.ToUpper("release")[:i%7+1]+"release"[i%7+1:]), "trace-import", daysAgo(i))
	}
	for i := 0; i < 5; i++ {
		insertMemory(t, db, fmt.Sprintf("unique-%d", i), "project", fmt.Sprintf("deploy release candidate %d", i), "trace-import", daysAgo(20+i))
	}
	results, err := Search(context.Background(), db, SearchOpts{Query: "deploy release", Limit: 5, Now: searchNow})
	if err != nil {
		t.Fatal(err)
	}
	dups := 0
	for _, r := range results {
		if strings.HasPrefix(r.ID, "dup-") {
			dups++
		}
	}
	if len(results) != 5 || dups != 1 {
		t.Fatalf("duplicates not collapsed or limit not filled: %v", resultIDs(results))
	}
	// Long memories that share a beginning but end differently are distinct.
	prefix := strings.Repeat("shared prompt words ", 200)
	insertMemory(t, db, "long-a", "long", prefix+"outcome alpha", "trace-import", daysAgo(1))
	insertMemory(t, db, "long-b", "long", prefix+"outcome beta", "trace-import", daysAgo(2))
	results, err = Search(context.Background(), db, SearchOpts{Query: "shared prompt", Scope: "long", Now: searchNow})
	if err != nil || len(results) != 2 || !results[0].ContentTruncated || len([]rune(results[0].Content)) != DefaultSnippetChars {
		t.Fatalf("distinct long memories collapsed or snippet unbounded: %v %v", resultIDs(results), err)
	}
}

func TestSearchFiltersAndRecentListing(t *testing.T) {
	db := searchTestDB(t)
	insertMemory(t, db, "a", "project", "shared keyword alpha", "manual", daysAgo(10))
	insertMemory(t, db, "b", "other", "shared keyword beta", "manual", daysAgo(5))
	insertMemory(t, db, "c", "project", "shared keyword gamma", "trace-import", daysAgo(1))
	insertMemory(t, db, "d", "project", "shared keyword delta", "trace-import", daysAgo(2))
	if _, err := db.Exec("INSERT INTO memory_imports VALUES('/trace/one.db','trace','turns','t','f','c'),('/trace/two.db','trace','turns','u','f','d')"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		opts SearchOpts
		want string
	}{
		{"scope", SearchOpts{Query: "keyword", Scope: "other"}, "b"},
		{"since", SearchOpts{Query: "keyword", Since: daysAgo(3)}, "c,d"},
		// Manual notes outrank equally relevant imports through the boost.
		{"source", SearchOpts{Query: "keyword", SourceDB: "/trace/one.db"}, "b,a,c"},
		{"limit", SearchOpts{Query: "keyword", Limit: 1}, "b"},
		{"recent", SearchOpts{RecentWhenEmpty: true}, "c,d,b,a"},
		{"blank without recent", SearchOpts{Query: ""}, ""},
		{"no words", SearchOpts{Query: "!!! ---", RecentWhenEmpty: true}, ""},
	} {
		tc.opts.Now = searchNow
		results, err := Search(ctx, db, tc.opts)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := strings.Join(resultIDs(results), ","); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
	if _, err := Search(ctx, db, SearchOpts{Query: strings.Repeat("x", MaxQueryBytes+1)}); err == nil {
		t.Fatal("oversized query accepted")
	}
}

func TestSearchTreatsFTSSyntaxAsLiteralWords(t *testing.T) {
	db := searchTestDB(t)
	insertMemory(t, db, "m", "project", "NOT a NEAR miss: content column OR star", "manual", daysAgo(1))
	for _, query := range []string{`"unterminated`, `NOT`, `NEAR(a b)`, `content:miss`, `mis*`, `a OR b AND`, `^start`, `(miss)`, `'quoted'`, `{content}: x`} {
		if _, err := Search(context.Background(), db, SearchOpts{Query: query, Now: searchNow}); err != nil {
			t.Fatalf("query %q interpreted as FTS syntax: %v", query, err)
		}
	}
	results, err := Search(context.Background(), db, SearchOpts{Query: "content:miss", Now: searchNow})
	if err != nil || len(results) != 1 {
		t.Fatalf("punctuation should separate literal words: %v %v", resultIDs(results), err)
	}
}

func TestSearchRedactsLegacyRowsOnRead(t *testing.T) {
	db := searchTestDB(t)
	secret := "ghp_" + strings.Repeat("a1B2", 9)
	insertMemory(t, db, "m", "scope "+secret, "lexicalterm "+secret, "manual", daysAgo(1))
	results, err := Search(context.Background(), db, SearchOpts{Query: "lexicalterm", Now: searchNow})
	if err != nil || len(results) != 1 || strings.Contains(results[0].Content, secret) || strings.Contains(results[0].Scope, secret) {
		t.Fatalf("legacy secret returned: %+v %v", results, err)
	}
}

func TestWritersMaintainContentFingerprint(t *testing.T) {
	db := searchTestDB(t)
	ctx := context.Background()
	fingerprint := func(id string) (string, string) {
		t.Helper()
		var content string
		var hash sql.NullString
		if err := db.QueryRow("SELECT content, content_hash FROM memories WHERE id=?", id).Scan(&content, &hash); err != nil {
			t.Fatal(err)
		}
		return hash.String, redact.Fingerprint(content)
	}
	if err := Remember(ctx, db, "m", "project", "first text", "manual", 1); err != nil {
		t.Fatal(err)
	}
	if got, want := fingerprint("m"); got != want {
		t.Fatalf("remember: %q want %q", got, want)
	}
	if err := Update(ctx, db, "m", "", "second text", 2); err != nil {
		t.Fatal(err)
	}
	if got, want := fingerprint("m"); got != want {
		t.Fatalf("update: %q want %q", got, want)
	}
	// Scope-only changes keep the fingerprint; content changes by writers
	// that do not maintain it clear it.
	if _, err := db.Exec("UPDATE memories SET scope='other' WHERE id='m'"); err != nil {
		t.Fatal(err)
	}
	if got, want := fingerprint("m"); got != want {
		t.Fatalf("scope update cleared fingerprint: %q", got)
	}
	if _, err := db.Exec("UPDATE memories SET content='third text' WHERE id='m'"); err != nil {
		t.Fatal(err)
	}
	if got, _ := fingerprint("m"); got != "" {
		t.Fatalf("external content change kept fingerprint %q", got)
	}
	if _, err := db.Exec("INSERT INTO memory_fts(memory_fts) VALUES('integrity-check')"); err != nil {
		t.Fatal(err)
	}
}

func TestSearchDedupKeepsMeaningfulPunctuationAndScopes(t *testing.T) {
	db := searchTestDB(t)
	for i, pair := range [][2]string{{"limit x > y", "limit x < y"}, {"limit is -1", "limit is 1"}, {"limit C++", "limit C"}} {
		insertMemory(t, db, fmt.Sprintf("a%d", i), "project", pair[0], "manual", daysAgo(1))
		insertMemory(t, db, fmt.Sprintf("b%d", i), "project", pair[1], "manual", daysAgo(1))
	}
	insertMemory(t, db, "same-text-other-scope", "other", "limit C", "manual", daysAgo(1))
	insertMemory(t, db, "case-dup", "project", "LIMIT   c", "manual", daysAgo(2))
	results, err := Search(context.Background(), db, SearchOpts{Query: "limit", Now: searchNow})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range results {
		got[r.ID] = true
	}
	for _, id := range []string{"a0", "b0", "a1", "b1", "a2", "b2", "same-text-other-scope"} {
		if !got[id] {
			t.Fatalf("%s merged with a different memory: %v", id, resultIDs(results))
		}
	}
	if got["case-dup"] || len(results) != 7 {
		t.Fatalf("case/whitespace duplicate not collapsed: %v", resultIDs(results))
	}
}

// Update after a change of marker digests only keeps the fingerprint set.
func TestUpdateWithMarkerOnlyChangeKeepsFingerprint(t *testing.T) {
	db := searchTestDB(t)
	ctx := context.Background()
	if err := Remember(ctx, db, "m", "project", "note ⟨redacted:env:11111111⟩", "manual", 1); err != nil {
		t.Fatal(err)
	}
	if err := Update(ctx, db, "m", "", "note ⟨redacted:env:22222222⟩", 2); err != nil {
		t.Fatal(err)
	}
	var hash sql.NullString
	if err := db.QueryRow("SELECT content_hash FROM memories WHERE id='m'").Scan(&hash); err != nil || hash.String != redact.Fingerprint("note ⟨redacted:env:22222222⟩") {
		t.Fatalf("fingerprint after marker-only update: %v %v", hash, err)
	}
}

func TestSearchDedupIgnoresMarkerDigests(t *testing.T) {
	db := searchTestDB(t)
	insertMemory(t, db, "a", "project", "rotate deploy key ⟨redacted:env:11111111⟩ today", "manual", daysAgo(1))
	insertMemory(t, db, "b", "project", "rotate deploy key ⟨redacted:env:22222222⟩ today", "manual", daysAgo(2))
	insertMemory(t, db, "c", "project", "rotate deploy key ⟨redacted:github-token:33333333⟩ today", "manual", daysAgo(3))
	results, err := Search(context.Background(), db, SearchOpts{Query: "rotate deploy", Now: searchNow})
	if err != nil || strings.Join(resultIDs(results), ",") != "a,c" {
		t.Fatalf("marker-digest-only duplicates not collapsed (different kinds kept): %v %v", resultIDs(results), err)
	}
}
