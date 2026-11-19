package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/UseTrace-SI/trace-brain/internal/store"
)

func TestRegressionCLIFactEvidenceIsBoundedLikeMCP(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		// Just above the 16 KiB output cap: large enough to exercise truncation
		// without paying for a 64 KiB capture on every CI repetition.
		{"ascii", strings.Repeat("a", 20000)},
		{"multibyte", strings.Repeat("中", 7000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "memory.db")
			db, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := store.Remember(ctx, db, "e", "project", tc.content, "manual", 1); err != nil {
				t.Fatal(err)
			}
			fact, err := store.AssertFact(ctx, db, store.Fact{Scope: "project", Subject: "A", Predicate: "knows", Object: "B", EvidenceID: "e", ValidFrom: 0}, 2)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			cmd := newRoot()
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{"--db", path, "facts", "evidence", fact.ID})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Evidence         store.FactEvidence `json:"evidence"`
				ContentBytes     int                `json:"content_bytes"`
				ContentTruncated bool               `json:"content_truncated"`
			}
			if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if !decoded.ContentTruncated || decoded.ContentBytes != len(tc.content) ||
				len(decoded.Evidence.Content) > 16384 ||
				!utf8.ValidString(decoded.Evidence.Content) {
				t.Fatalf("unbounded/invalid CLI evidence: bytes=%d truncated=%v content=%d",
					decoded.ContentBytes, decoded.ContentTruncated, len(decoded.Evidence.Content))
			}
			if decoded.Evidence.EvidenceHash != fact.EvidenceHash {
				t.Fatal("capture hash lost")
			}
		})
	}
}

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
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(ctx, db, "e", "", "edited evidence", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := json.Unmarshal(run("facts", "query", "--subject", "A", "--predicate", "uses"), &graph); err != nil || len(graph.Result.Facts) != 0 {
		t.Fatal("current evidence change ignored", err)
	}
	if err := json.Unmarshal(run("facts", "query", "--subject", "A", "--predicate", "uses", "--evidence-mode", "captured"), &graph); err != nil || len(graph.Result.Facts) != 1 || !graph.Result.Facts[0].EvidenceStale || graph.Result.EvidenceMode != "captured" {
		t.Fatalf("captured mode: %+v %v", graph, err)
	}
	var capture struct {
		Evidence store.FactEvidence `json:"evidence"`
	}
	if err := json.Unmarshal(run("facts", "evidence", asserted.Fact.ID), &capture); err != nil || capture.Evidence.Content != "deployment evidence" || capture.Evidence.SourceVerified {
		t.Fatalf("evidence: %+v %v", capture, err)
	}
	if err := json.Unmarshal(run("facts", "graph", "A", "--depth", "3", "--direction", "both", "--evidence-mode", "captured"), &graph); err != nil || len(graph.Result.Facts) != 4 {
		t.Fatalf("captured graph: %+v %v", graph, err)
	}
	run("facts", "retract", asserted.Fact.ID)
	if err := json.Unmarshal(run("facts", "query", "--subject", "A", "--predicate", "uses"), &graph); err != nil || len(graph.Result.Facts) != 0 {
		t.Fatalf("retract: %+v %v", graph, err)
	}
	run("facts", "forget", asserted.Fact.ID)
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := store.Get(ctx, db, "e"); err != nil {
		t.Fatal("claim deletion removed its source", err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM fact_evidence WHERE fact_id=?", asserted.Fact.ID).Scan(&n); err != nil || n != 0 {
		t.Fatal("claim deletion retained captured evidence", err)
	}
}
