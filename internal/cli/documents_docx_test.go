package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-brain/internal/store"
)

func writeDOCX(t *testing.T, file, text string) []byte {
	t.Helper()
	var output bytes.Buffer
	w := zip.NewWriter(&output)
	for name, content := range map[string]string{
		"[Content_Types].xml": `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
		"_rels/.rels":         `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="main" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`,
		"word/document.xml":   `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>` + text + `</w:t></w:r></w:p></w:body></w:document>`,
	} {
		part, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, output.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestRegressionDOCXCollectionUsesExistingExclusionsBeforeParsing(t *testing.T) {
	root := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", output, err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored.docx\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"ignored.docx", "secrets.docx", ".env.docx", "~$guide.docx"} {
		if err := os.WriteFile(filepath.Join(root, file), []byte("not a ZIP"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	original := writeDOCX(t, filepath.Join(root, "guide.DOCX"), "officekeyword ghp_abcdefghijklmnopqrstuvwxyz")
	if err := os.Symlink("guide.DOCX", filepath.Join(root, "linked.docx")); err != nil {
		t.Fatal(err)
	}
	docs, excluded, err := collectDocuments(context.Background(), root)
	if err != nil || len(docs) != 1 || excluded < 4 || docs[0].Path != "guide.DOCX" || strings.Contains(docs[0].Content, "ghp_abcdefghijklmnopqrstuvwxyz") || !strings.Contains(docs[0].Content, "officekeyword") {
		t.Fatalf("unsafe DOCX collection: %+v %d %v", docs, excluded, err)
	}
	current, err := os.ReadFile(filepath.Join(root, "guide.DOCX"))
	if err != nil || !bytes.Equal(current, original) {
		t.Fatal("source package was modified", err)
	}
}

func TestRegressionDOCXSyncRefreshRollbackAndPrunePreserveManualMemories(t *testing.T) {
	root := t.TempDir()
	database := filepath.Join(t.TempDir(), "m.db")
	file := filepath.Join(root, "guide.docx")
	writeDOCX(t, file, "originalofficekeyword")
	run := func(args ...string) ([]byte, error) {
		var output bytes.Buffer
		command := newRoot()
		command.SetOut(&output)
		command.SetErr(&bytes.Buffer{})
		command.SetArgs(append([]string{"--db", database}, args...))
		err := command.Execute()
		return output.Bytes(), err
	}
	if _, err := run("documents", "sync", "--root", root, "--dry-run"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(database); !os.IsNotExist(err) {
		t.Fatal("DOCX preview created state", err)
	}
	if _, err := run("remember", "manual note"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("documents", "sync", "--root", root); err != nil {
		t.Fatal(err)
	}
	before, err := run("documents", "list", "--root", root)
	if err != nil {
		t.Fatal(err)
	}
	writeDOCX(t, file, "updatedofficekeyword")
	if err := os.WriteFile(filepath.Join(root, "zzz-bad.docx"), []byte("not a ZIP"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := run("documents", "sync", "--root", root, "--prune"); err == nil {
		t.Fatal("malformed DOCX was committed")
	}
	after, err := run("documents", "list", "--root", root)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed extraction changed the previous source records", err)
	}
	db, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	var original int
	if err := db.QueryRow("SELECT count(*) FROM memories WHERE content LIKE '%originalofficekeyword%'").Scan(&original); err != nil || original != 1 {
		t.Fatal("failed scan changed prior text", err)
	}
	db.Close()
	if err := os.Remove(filepath.Join(root, "zzz-bad.docx")); err != nil {
		t.Fatal(err)
	}
	refreshed, err := run("documents", "sync", "--root", root)
	if err != nil || !bytes.Contains(refreshed, []byte(`"imported":1`)) {
		t.Fatalf("DOCX refresh: %s %v", refreshed, err)
	}
	var records []store.DocumentRecord
	listed, err := run("documents", "list", "--root", root)
	if err != nil || json.Unmarshal(listed, &records) != nil || len(records) != 1 {
		t.Fatal("DOCX record disappeared", err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	pruned, err := run("documents", "sync", "--root", root, "--prune")
	if err != nil || !bytes.Contains(pruned, []byte(`"pruned":1`)) {
		t.Fatalf("DOCX prune: %s %v", pruned, err)
	}
	db, err = store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var manual, derived int
	if err := db.QueryRow("SELECT count(*) FROM memories WHERE source='manual'").Scan(&manual); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM memories WHERE source='document'").Scan(&derived); err != nil || manual != 1 || derived != 0 {
		t.Fatalf("wrong deletion scope: manual %d derived %d %v", manual, derived, err)
	}
}
