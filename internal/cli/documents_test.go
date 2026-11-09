package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocumentCollectionExcludesSecretsIgnoredFilesAndLinksBeforeReading(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("guide.md", "docs keyword sk-abcdefghijklmnopqrstuvwxyz1234567890")
	write(".gitignore", "ignored.txt\n")
	write("ignored.txt", string([]byte{0xff, 0x00}))
	write("secrets.md", "private")
	write(".env.txt", "private")
	write("binary.js", "not a document")
	if err := os.Symlink("guide.md", filepath.Join(root, "linked.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	write(".ssh/key.md", "private")
	documents, excluded, err := collectDocuments(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].Path != "guide.md" || excluded < 4 {
		t.Fatalf("unsafe selection: %+v excluded %d", documents, excluded)
	}
	if strings.Contains(documents[0].Content, "sk-abcdefghijklmnopqrstuvwxyz") {
		t.Fatal("unredacted content")
	}
	anchor, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer anchor.Close()
	if file, err := openDocument(anchor, "linked.md"); err == nil {
		file.Close()
		t.Fatal("followed link")
	}
	write("bad.txt", string([]byte{0xff}))
	if _, _, err := collectDocuments(context.Background(), root); err == nil {
		t.Fatal("accepted binary text")
	}
}

func TestDocumentsCLIPreviewDoesNotCreateAnIndexAndSyncTracksDeletion(t *testing.T) {
	root := t.TempDir()
	database := filepath.Join(t.TempDir(), "memory.db")
	if err := os.WriteFile(filepath.Join(root, "guide.md"), []byte("test document"), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) []byte {
		t.Helper()
		var output bytes.Buffer
		cmd := newRoot()
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs(append([]string{"--db", database}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %s %v", args, &output, err)
		}
		return output.Bytes()
	}
	preview := run("documents", "sync", "--root", root, "--dry-run")
	if !bytes.Contains(preview, []byte(`"database_changed":false`)) {
		t.Fatalf("invalid preview: %s", preview)
	}
	if _, err := os.Stat(database); !os.IsNotExist(err) {
		t.Fatal("preview created an index")
	}
	run("documents", "sync", "--root", root)
	var records []map[string]any
	if err := json.Unmarshal(run("documents", "list", "--root", root), &records); err != nil || len(records) != 1 {
		t.Fatalf("list: %+v %v", records, err)
	}
	if err := os.Remove(filepath.Join(root, "guide.md")); err != nil {
		t.Fatal(err)
	}
	result := run("documents", "sync", "--root", root, "--prune")
	if !bytes.Contains(result, []byte(`"pruned":1`)) {
		t.Fatalf("missing prune: %s", result)
	}
	if err := json.Unmarshal(run("documents", "list", "--root", root), &records); err != nil || len(records) != 0 {
		t.Fatalf("deleted source remains: %+v %v", records, err)
	}
}

func TestDeletedDocumentSourcesRetainCanonicalIdentityThroughRootAliases(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "docs")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink("docs", alias); err != nil {
		t.Fatal(err)
	}
	original, err := documentRoot(alias)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	missing, err := documentRootPath(alias, false)
	if err != nil || missing != original {
		t.Fatalf("lost source identity: %q != %q, %v", missing, original, err)
	}
	if _, err := documentRoot(alias); err == nil {
		t.Fatal("sync accepted a missing source")
	}
}
