package mcp

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/store"
)

var benchVocabulary = strings.Fields(`the a an and or of to in on for with is are was were be been it this that we they
build test tests testing flaky ci pipeline deploy release database migration schema index query sqlite postgres
cache redis timeout retry deadline authentication token session login logout user account password reset
api endpoint handler router middleware request response error panic crash fix bug regression patch refactor
decided decide decision chose prefer use using used switched replaced removed added dependency module package
frontend backend server client worker queue job scheduler cron metrics logging tracing alert dashboard latency
memory leak goroutine channel mutex lock race detector benchmark profile allocation garbage collector heap
config flag environment variable secret vault docker kubernetes cluster node pod container image registry
review merge branch commit rebase conflict tag version changelog docs readme onboarding guide tutorial
payment invoice billing subscription customer order cart checkout inventory product search ranking recall`)

// BenchmarkMCPRecall20k measures the warm brain_recall handler against 20,000
// synthetic memories (20% manual, spread over a year).
func BenchmarkMCPRecall20k(b *testing.B) {
	db, err := store.Open(filepath.Join(b.TempDir(), "memory.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	rng := rand.New(rand.NewSource(42))
	now := time.Now().UnixMilli()
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 20000; i++ {
		words := make([]string, 8+rng.Intn(60))
		for j := range words {
			words[j] = benchVocabulary[rng.Intn(len(benchVocabulary))]
		}
		source := "trace-import"
		if i%5 == 0 {
			source = "manual"
		}
		if _, err := tx.Exec("INSERT INTO memories(id,scope,content,source,created_at) VALUES(?,?,?,?,?)", fmt.Sprintf("mem-%06d", i), "project", strings.Join(words, " "), source, now-rng.Int63n(365*24*3600*1000)); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	s := New(db)
	for _, query := range []string{"flaky tests", "decide database migration", "the database", "authentication timeout retry"} {
		b.Run(strings.ReplaceAll(query, " ", "_"), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, _, err := s.recall(context.Background(), nil, recallInput{Query: query}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
