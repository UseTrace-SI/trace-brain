package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/UseTrace-SI/trace-brain/internal/store"
)

type fakeLocalModel struct {
	digest func() string
	embed  func([]string) [][]float32
}

func (m *fakeLocalModel) Origin() string { return "http://127.0.0.1:11434" }
func (m *fakeLocalModel) Close()         {}
func (m *fakeLocalModel) Digest(ctx context.Context, model string) (string, error) {
	return m.digest(), ctx.Err()
}
func (m *fakeLocalModel) Embed(ctx context.Context, model string, texts []string) ([][]float32, error) {
	return m.embed(texts), ctx.Err()
}

func TestLearnedEmbeddingRetrievalTracksModelAndContentIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for id, content := range map[string]string{"car": "a car has four wheels sk-abcdefghijklmnopqrstuvwxyz1234567890", "jobs": "background queue worker"} {
		if err := store.Remember(context.Background(), db, id, "project", content, "manual", 1); err != nil {
			t.Fatal(err)
		}
	}
	var digest atomic.Value
	digest.Store(strings.Repeat("a", 64))
	var embedded atomic.Int64
	provider := &fakeLocalModel{digest: func() string { return digest.Load().(string) }, embed: func(texts []string) [][]float32 {
		vectors := [][]float32{}
		for _, text := range texts {
			if strings.Contains(text, "sk-abcdefghijklmnopqrstuvwxyz") {
				t.Error("sent an unredacted token to the model")
			}
			if strings.Contains(text, "car") || strings.Contains(text, "automobile") {
				vectors = append(vectors, []float32{1, 0})
			} else {
				vectors = append(vectors, []float32{0, 1})
			}
		}
		embedded.Add(int64(len(texts)))
		return vectors
	}}
	factory := func(string) (modelProvider, error) { return provider, nil }
	run := func(name string, args ...string) (map[string]any, error) {
		var out bytes.Buffer
		cmd := newRootWithModelProvider(factory)
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		selected := []string{"--db", path, name, "--model", "test:1"}
		if name == "semantic" {
			selected = append(selected, "--min-score", "0.5")
		}
		cmd.SetArgs(append(selected, args...))
		err := cmd.Execute()
		var result map[string]any
		if err == nil {
			err = json.Unmarshal(out.Bytes(), &result)
		}
		return result, err
	}
	if got, err := run("embed"); err != nil || got["embedded"] != float64(2) {
		t.Fatalf("index: %+v %v", got, err)
	}
	if got, err := run("embed"); err != nil || got["unchanged"] != float64(2) || embedded.Load() != 2 {
		t.Fatalf("unchanged memories were embedded again: %+v %v", got, err)
	}
	got, err := run("semantic", "automobile")
	if err != nil {
		t.Fatal(err)
	}
	memories := got["memories"].([]any)
	if len(memories) != 1 || memories[0].(map[string]any)["id"] != "car" {
		t.Fatalf("non-lexical vector match was lost: %+v", got)
	}
	if _, err := db.Exec("UPDATE memories SET content='changed source' WHERE id='car'"); err != nil {
		t.Fatal(err)
	}
	got, err = run("semantic", "automobile")
	if err != nil || got["stale_vectors"] != float64(1) || len(got["memories"].([]any)) != 0 {
		t.Fatalf("stale vector was used: %+v %v", got, err)
	}
	digest.Store(strings.Repeat("b", 64))
	got, err = run("semantic", "automobile")
	if err != nil || got["status"] != "needs_embeddings" || got["missing_vectors"] != float64(2) {
		t.Fatalf("mixed model versions: %+v %v", got, err)
	}
}

func TestEmbeddingModelChangesDuringInferenceDoNotCommitVectors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := store.Remember(context.Background(), db, "m", "project", "memory", "manual", 1); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	provider := &fakeLocalModel{digest: func() string {
		value := "a"
		if calls.Add(1) > 1 {
			value = "b"
		}
		return strings.Repeat(value, 64)
	}, embed: func([]string) [][]float32 { return [][]float32{{1, 0}} }}
	cmd := newRootWithModelProvider(func(string) (modelProvider, error) { return provider, nil })
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", path, "embed", "--model", "test:1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("accepted changing model")
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM semantic_embeddings").Scan(&count); err != nil || count != 0 {
		t.Fatalf("committed mixed vectors: %d %v", count, err)
	}
}
