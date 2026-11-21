package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-brain/internal/embed"
	"github.com/UseTrace-SI/trace-brain/internal/store"
)

// context output is consumed (and can be published) by Trace, yet it and
// workspace retrieval returned stored rows without the read-time redaction
// already applied by recall/query/brief/dash/get/history. A legacy row that
// escaped write-time redaction leaked through them unchanged.
func TestRegressionContextWorkspaceAndVizReapplyRedactionOnRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	secret := runtimeCredential()
	content := "lexicalterm " + secret
	if _, err := db.Exec("INSERT INTO memories(id,scope,content,source,created_at) VALUES('m',?,?,'manual',1)", "scope "+secret, content); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO embeddings(memory_id,dim,vector) VALUES('m',?,?)", embed.Dim, embed.Encode(embed.Embed(content))); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"context", "--query", "lexicalterm"},
		{"context", "--semantic", "--query", "lexicalterm"},
		{"context"},
		{"workspace", "query", "lexicalterm", "--databases", path},
		{"viz"},
	} {
		var out bytes.Buffer
		cmd := newRoot()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--db", path}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		got := out.String()
		if args[0] == "viz" {
			page, err := os.ReadFile(filepath.Join(dir, "viz.html"))
			if err != nil {
				t.Fatal(err)
			}
			got = string(page)
		}
		if strings.Contains(got, secret) || !strings.Contains(got, "redacted:") {
			t.Fatalf("%v leaked unredacted stored text: %s", args, got)
		}
	}
}
