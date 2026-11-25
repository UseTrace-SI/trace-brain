package cli

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/store"
)

// benchVocabulary mixes ordinary engineering words with common stopwords so
// a synthetic corpus has realistic term frequencies for BM25.
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

// syntheticMemoryDB creates n memories with deterministic content spread over a
// year, 20% manual and 80% imported, through the current schema.
func syntheticMemoryDB(tb testing.TB, n int) string {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "memory.db")
	db, err := store.Open(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer db.Close()
	rng := rand.New(rand.NewSource(42))
	now := time.Now().UnixMilli()
	tx, err := db.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	stmt, err := tx.Prepare("INSERT INTO memories(id,scope,content,source,session_id,turn_id,created_at) VALUES(?,?,?,?,?,?,?)")
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < n; i++ {
		words := make([]string, 8+rng.Intn(60))
		for j := range words {
			words[j] = benchVocabulary[rng.Intn(len(benchVocabulary))]
		}
		source, session, turn := "trace-import", fmt.Sprintf("s%d", i/10), fmt.Sprintf("t%d", i)
		if i%5 == 0 {
			source, session, turn = "manual", "", ""
		}
		created := now - rng.Int63n(365*24*3600*1000)
		if _, err := stmt.Exec(fmt.Sprintf("mem-%06d", i), "project", strings.Join(words, " "), source, session, turn, created); err != nil {
			tb.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	return path
}

var benchQueries = []string{"flaky tests", "decide database migration", "the database", "authentication timeout retry"}

// BenchmarkRecallCLI20k measures end-to-end `recall` (open, migrate check,
// search and print) against 20,000 memories.
func BenchmarkRecallCLI20k(b *testing.B) {
	path := syntheticMemoryDB(b, 20000)
	for _, query := range benchQueries {
		b.Run(strings.ReplaceAll(query, " ", "_"), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				var out bytes.Buffer
				cmd := newRoot()
				cmd.SetOut(&out)
				cmd.SetArgs([]string{"--db", path, "recall", query})
				if err := cmd.ExecuteContext(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkContextQuery20k measures the read-only `context` JSON path.
func BenchmarkContextQuery20k(b *testing.B) {
	path := syntheticMemoryDB(b, 20000)
	for _, query := range benchQueries {
		b.Run(strings.ReplaceAll(query, " ", "_"), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				var out bytes.Buffer
				cmd := newRoot()
				cmd.SetOut(&out)
				cmd.SetArgs([]string{"--db", path, "context", "--trace-db", "", "--query", query})
				if err := cmd.ExecuteContext(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
