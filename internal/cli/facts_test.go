package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/UseTrace-SI/trace-brain/internal/store"
)

func TestFactsCLIQueriesAndTraversesScopedTemporalClaims(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Remember(ctx, db, "e", "project", "deployment evidence", "manual", 1); err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][3]string{{"A", "calls", "B"}, {"B", "calls", "C"}, {"C", "calls", "A"}} {
		if _, err := store.AssertFact(ctx, db, store.Fact{Scope: "project", Subject: edge[0], Predicate: edge[1], Object: edge[2], EvidenceID: "e", ValidFrom: 0}, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.AssertFact(ctx, db, store.Fact{Scope: "other", Subject: "A", Predicate: "calls", Object: "outside", EvidenceID: "e", ValidFrom: 0}, 1); err != nil {
		t.Fatal(err)
	}
	db.Close()
	run := func(args ...string) []byte {
		t.Helper()
		var output bytes.Buffer
		cmd := newRoot()
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs(append([]string{"--db", path}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %s %v", args, &output, err)
		}
		return output.Bytes()
	}
	var graph struct {
		Result store.FactResults `json:"result"`
	}
	if err := json.Unmarshal(run("facts", "graph", "A", "--depth", "3"), &graph); err != nil || len(graph.Result.Facts) != 3 || graph.Result.SourceVerified {
		t.Fatalf("cyclic traversal: %+v %v", graph, err)
	}
	if err := json.Unmarshal(run("facts", "graph", "A", "--depth", "1", "--direction", "both"), &graph); err != nil || len(graph.Result.Facts) != 2 {
		t.Fatalf("direction traversal: %+v %v", graph, err)
	}
	var asserted struct {
		Fact store.Fact `json:"fact"`
	}
	if err := json.Unmarshal(run("facts", "assert", "A", "uses", "sqlite", "--evidence", "e", "--from", "2024-01-01T00:00:00Z", "--exclusive"), &asserted); err != nil || asserted.Fact.ID == "" {
		t.Fatalf("assert: %+v %v", asserted, err)
	}
	if err := json.Unmarshal(run("facts", "query", "--subject", "A", "--predicate", "uses", "--at", "2024-02-01T00:00:00Z"), &graph); err != nil || len(graph.Result.Facts) != 1 {
		t.Fatalf("query: %+v %v", graph, err)
	}
	run("facts", "retract", asserted.Fact.ID)
	if err := json.Unmarshal(run("facts", "query", "--subject", "A", "--predicate", "uses"), &graph); err != nil || len(graph.Result.Facts) != 0 {
		t.Fatalf("retract: %+v %v", graph, err)
	}
}
