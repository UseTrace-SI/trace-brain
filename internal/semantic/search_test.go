package semantic

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/UseTrace-SI/trace-brain/internal/embed"
	"github.com/UseTrace-SI/trace-brain/internal/store"
)

type searchModel struct {
	digest func(context.Context) (string, error)
	embed  func(context.Context, []string) ([][]float32, error)
}

func (*searchModel) Origin() string { return "http://127.0.0.1:11434" }
func (*searchModel) Close()         {}
func (m *searchModel) Digest(ctx context.Context, _ string) (string, error) {
	if m.digest != nil {
		return m.digest(ctx)
	}
	return strings.Repeat("a", 64), ctx.Err()
}
func (m *searchModel) Embed(ctx context.Context, _ string, texts []string) ([][]float32, error) {
	if m.embed != nil {
		return m.embed(ctx, texts)
	}
	return [][]float32{{1, 0}}, ctx.Err()
}
func searchDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func searchSeed(t *testing.T, db *sql.DB, id, scope, text string, created int64, vector []float32) {
	t.Helper()
	if err := store.Remember(context.Background(), db, id, scope, text, "manual", created); err != nil {
		t.Fatal(err)
	}
	if vector == nil {
		return
	}
	if _, err := db.Exec(`INSERT INTO semantic_embeddings(memory_id,endpoint,model,digest,content_hash,dim,vector) VALUES(?,?,?,?,?,?,?)`, id, "http://127.0.0.1:11434", "test:1", strings.Repeat("a", 64), fmt.Sprintf("%x", sha256.Sum256([]byte(text))), len(vector), embed.Encode(vector)); err != nil {
		t.Fatal(err)
	}
}
func searchRequest() SearchRequest {
	return SearchRequest{Query: "automobile", Model: "test:1", Scope: "project", Limit: 20, MinScore: 0.5}
}

func TestRegressionSemanticSearchScopesRanksAndRejectsStaleIdentities(t *testing.T) {
	db := searchDB(t)
	searchSeed(t, db, "car-a", "project", "a car", 2, []float32{1, 0})
	searchSeed(t, db, "car-b", "project", "a car", 2, []float32{1, 0})
	searchSeed(t, db, "old-car", "project", "a car", 1, []float32{1, 0})
	searchSeed(t, db, "queue", "project", "background job", 9, []float32{0, 1})
	searchSeed(t, db, "other", "other", "a car", 99, []float32{1, 0})
	searchSeed(t, db, "missing", "project", "a car", 99, nil)
	searchSeed(t, db, "stale", "project", "a car", 99, []float32{1, 0})
	searchSeed(t, db, "zero", "project", "a car", 99, []float32{0, 0})
	searchSeed(t, db, "nan", "project", "a car", 99, []float32{float32(math.NaN()), 1})
	searchSeed(t, db, "wrong-endpoint", "project", "a car", 99, []float32{1, 0})
	if _, err := db.Exec("UPDATE memories SET content='source changed' WHERE id='stale'; UPDATE semantic_embeddings SET endpoint='http://localhost:11434' WHERE memory_id='wrong-endpoint'; PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	out, err := Search(context.Background(), db, &searchModel{}, searchRequest())
	if err != nil || out.Status != "ready" || out.IndexedMemories != 4 || out.StaleVectors != 3 || out.MissingVectors != 2 || len(out.Memories) != 3 {
		t.Fatalf("wrong identity/rank result: %+v %v", out, err)
	}
	for i, want := range []string{"car-a", "car-b", "old-car"} {
		if out.Memories[i].ID != want || out.Memories[i].Score != 1 {
			t.Fatalf("unstable rank: %+v", out.Memories)
		}
	}
	provider := &searchModel{digest: func(ctx context.Context) (string, error) { return strings.Repeat("b", 64), ctx.Err() }}
	out, err = Search(context.Background(), db, provider, searchRequest())
	if err != nil || out.Status != "needs_embeddings" || out.MissingVectors != 9 || len(out.Memories) != 0 {
		t.Fatalf("reused a different model digest: %+v %v", out, err)
	}
}

func TestRegressionSemanticSearchRedactsQueriesAndBoundsUnicodeAndJSON(t *testing.T) {
	db := searchDB(t)
	secret := "ghp_abcdefghijklmnopqrstuvwxyz"
	legacy := secret + " " + strings.Repeat("中", 1000)
	searchSeed(t, db, "legacy", "unicode", "placeholder", 1, nil)
	if _, err := db.Exec("UPDATE memories SET content=? WHERE id='legacy'", legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO semantic_embeddings VALUES(?,?,?,?,?,?,?)", "legacy", "http://127.0.0.1:11434", "test:1", strings.Repeat("a", 64), fmt.Sprintf("%x", sha256.Sum256([]byte(legacy))), 2, embed.Encode([]float32{1, 0})); err != nil {
		t.Fatal(err)
	}
	provider := &searchModel{embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		if len(texts) != 1 || strings.Contains(texts[0], secret) {
			t.Fatal("query secret reached model")
		}
		return [][]float32{{1, 0}}, ctx.Err()
	}}
	in := searchRequest()
	in.Query, in.Scope = "automobile "+secret, "unicode"
	out, err := Search(context.Background(), db, provider, in)
	if err != nil || len(out.Memories) != 1 {
		t.Fatalf("unicode query: %+v %v", out, err)
	}
	item := out.Memories[0]
	if !item.ContentTruncated || !utf8.ValidString(item.Content) || len(item.Content) > 2048 || strings.Contains(item.Content, secret) {
		t.Fatalf("invalid snippet bounds/redaction: %+v", item)
	}
	for i := 0; i < 50; i++ {
		searchSeed(t, db, fmt.Sprintf("large-%02d", i), "large", strings.Repeat("<", 3000), 1, []float32{1, 0})
	}
	in.Scope, in.Limit = "large", 50
	out, err = Search(context.Background(), db, provider, in)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(out)
	if err != nil || !out.ResultTruncated || len(raw) > 64<<10 || len(out.Memories) == 0 || len(out.Memories) >= 50 {
		t.Fatalf("unbounded encoded payload: %d %d %v", len(raw), len(out.Memories), err)
	}
}

func TestRegressionSemanticSearchRejectsInvalidInputAndChangingModels(t *testing.T) {
	db := searchDB(t)
	searchSeed(t, db, "m", "project", "a car", 1, []float32{1, 0})
	for _, mutate := range []func(*SearchRequest){
		func(in *SearchRequest) { in.Query = "" }, func(in *SearchRequest) { in.Query = strings.Repeat("x", 4097) },
		func(in *SearchRequest) { in.Limit = 51 }, func(in *SearchRequest) { in.Scope = strings.Repeat("s", 257) },
		func(in *SearchRequest) { in.Model = "test\n1" }, func(in *SearchRequest) { in.MinScore = math.NaN() },
	} {
		in := searchRequest()
		mutate(&in)
		provider := &searchModel{digest: func(context.Context) (string, error) { t.Fatal("invalid request contacted model"); return "", nil }}
		if _, err := Search(context.Background(), db, provider, in); err == nil {
			t.Fatal("accepted invalid query")
		}
	}
	for _, vector := range [][][]float32{nil, {{0, 0}}, {{float32(math.Inf(1)), 0}}, {{1}, {1}}} {
		provider := &searchModel{embed: func(context.Context, []string) ([][]float32, error) { return vector, nil }}
		if _, err := Search(context.Background(), db, provider, searchRequest()); err == nil {
			t.Fatal("invalid query vector accepted")
		}
	}
	calls := 0
	provider := &searchModel{digest: func(context.Context) (string, error) {
		calls++
		return strings.Repeat(fmt.Sprintf("%x", calls), 64), nil
	}}
	if _, err := Search(context.Background(), db, provider, searchRequest()); err == nil {
		t.Fatal("changing model accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Search(ctx, db, &searchModel{}, searchRequest()); err == nil {
		t.Fatal("cancelled query succeeded")
	}
}

func TestRegressionSemanticLegacyReadDoesNotMigrateOrWrite(t *testing.T) {
	db := searchDB(t)
	searchSeed(t, db, "m", "project", "a car", 1, nil)
	if _, err := db.Exec("DROP TABLE semantic_embeddings; PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	out, err := Search(context.Background(), db, &searchModel{}, searchRequest())
	if err != nil || out.Status != "needs_embeddings" || out.MissingVectors != 1 || len(out.Memories) != 0 {
		t.Fatalf("legacy read: %+v %v", out, err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='semantic_embeddings'").Scan(&count); err != nil || count != 0 {
		t.Fatal("read changed schema", err)
	}
}

func TestSemanticScanCapFailsWithoutPartialRanking(t *testing.T) {
	db := searchDB(t)
	if _, err := db.Exec(`WITH RECURSIVE seq(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM seq WHERE x<100001) INSERT INTO memories(id,scope,content,created_at) SELECT 'id-'||x,'large','x',1 FROM seq`); err != nil {
		t.Fatal(err)
	}
	in := searchRequest()
	in.Scope = "large"
	if _, err := Search(context.Background(), db, &searchModel{}, in); err == nil || !strings.Contains(err.Error(), "100000") {
		t.Fatal("unbounded scan accepted", err)
	}
	if _, err := db.Exec("DROP TABLE semantic_embeddings; PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := Search(context.Background(), db, &searchModel{}, in); err == nil || !strings.Contains(err.Error(), "100000") {
		t.Fatal("legacy scan cap missing", err)
	}
}
