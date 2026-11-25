package semantic

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-brain/internal/embed"
	"github.com/UseTrace-SI/trace-brain/internal/store"
)

type benchProvider struct{ vector []float32 }

func (*benchProvider) Origin() string { return "http://127.0.0.1:11434" }
func (*benchProvider) Close()         {}
func (*benchProvider) Digest(ctx context.Context, _ string) (string, error) {
	return strings.Repeat("a", 64), ctx.Err()
}
func (p *benchProvider) Embed(ctx context.Context, _ string, _ []string) ([][]float32, error) {
	return [][]float32{p.vector}, ctx.Err()
}

// BenchmarkLearnedSearch20k measures a learned query over 20,000 memories with
// 384-dimension cached vectors and ~1 KiB of content each.
func BenchmarkLearnedSearch20k(b *testing.B) {
	const dim = 384
	db, err := store.Open(filepath.Join(b.TempDir(), "memory.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	rng := rand.New(rand.NewSource(7))
	random := func() []float32 {
		v := make([]float32, dim)
		for i := range v {
			v[i] = rng.Float32()*2 - 1
		}
		return v
	}
	ctx := context.Background()
	// Releases that store content fingerprints set them on every write, as
	// store.Remember does; older schemas have no such column.
	var hashColumn int
	if err := db.QueryRow("SELECT count(*) FROM pragma_table_info('memories') WHERE name='content_hash'").Scan(&hashColumn); err != nil {
		b.Fatal(err)
	}
	for start := 0; start < 20000; start += 1000 {
		tx, err := db.Begin()
		if err != nil {
			b.Fatal(err)
		}
		for i := start; i < start+1000; i++ {
			id := fmt.Sprintf("mem-%06d", i)
			content := fmt.Sprintf("memory %d %s", i, strings.Repeat("deploy pipeline notes ", 48))
			if _, err := tx.Exec("INSERT INTO memories(id,scope,content,source,created_at) VALUES(?,?,?,?,?)", id, "project", content, "manual", i); err != nil {
				b.Fatal(err)
			}
			if hashColumn > 0 {
				if _, err := tx.Exec("UPDATE memories SET content_hash=? WHERE id=?", ContentHash(content), id); err != nil {
					b.Fatal(err)
				}
			}
			if _, err := tx.Exec("INSERT INTO semantic_embeddings VALUES(?,?,?,?,?,?,?)", id, "http://127.0.0.1:11434", "test:1", strings.Repeat("a", 64), ContentHash(content), dim, embed.Encode(random())); err != nil {
				b.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			b.Fatal(err)
		}
	}
	provider := &benchProvider{vector: random()}
	request := SearchRequest{Query: "deploy", Model: "test:1", Limit: 20, MinScore: -1}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := Search(ctx, db, provider, request)
		if err != nil || len(out.Memories) != 20 {
			b.Fatal(out.Status, err)
		}
	}
}
