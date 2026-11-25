package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/store"
)

// recall, query, brief, context and workspace query all rank through
// store.Search, so the same database and words must yield the same order.
func TestKeywordSurfacesShareOneRanking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, content := range []string{
		"deploy pipeline timeout during release",
		"release notes for the deploy",
		"Release  notes for the DEPLOY", // near-duplicate of the previous note
		"deploy",
		"unrelated stylesheet colors",
	} {
		at := now.Add(-time.Duration(i) * 24 * time.Hour).UnixMilli()
		if err := store.Remember(context.Background(), db, fmt.Sprintf("m%d", i), "project", content, "manual", at); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		cmd := newRoot()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--db", path}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	idPattern := regexp.MustCompile(`\[(m\d)\]`)
	var recall []string
	for _, match := range idPattern.FindAllStringSubmatch(run("recall", "deploy", "release"), -1) {
		recall = append(recall, match[1])
	}
	if len(recall) != 3 {
		t.Fatalf("near-duplicate not collapsed or match missing: %v", recall)
	}
	var query []string
	for _, match := range idPattern.FindAllStringSubmatch(run("query", "deploy", "release"), -1) {
		query = append(query, match[1])
	}
	var ctxOut memoryContext
	if err := json.Unmarshal([]byte(run("context", "--trace-db", "", "--query", "deploy release")), &ctxOut); err != nil {
		t.Fatal(err)
	}
	var contextIDs []string
	for _, m := range ctxOut.Memories {
		contextIDs = append(contextIDs, m.ID)
	}
	var workspace struct {
		Memories []workspaceMemory `json:"memories"`
	}
	if err := json.Unmarshal([]byte(run("workspace", "query", "deploy release", "--databases", path)), &workspace); err != nil {
		t.Fatal(err)
	}
	var workspaceIDs []string
	for _, m := range workspace.Memories {
		workspaceIDs = append(workspaceIDs, m.ID)
	}
	want := strings.Join(recall, ",")
	for name, got := range map[string][]string{"query": query, "context": contextIDs, "workspace": workspaceIDs} {
		if strings.Join(got, ",") != want {
			t.Fatalf("%s ranking %v differs from recall %v", name, got, recall)
		}
	}
	if brief := run("brief", "deploy", "release"); strings.Count(brief, "\n- ") != 3 || !strings.HasPrefix(brief, "Brief for: deploy release\n") {
		t.Fatalf("brief diverged: %q", brief)
	}
}
