package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/UseTrace-SI/trace-brain/internal/store"
)

func TestMemoryCLIUpdatesHistoryVerificationAndDeletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	source := filepath.Join(t.TempDir(), "trace.db")
	src, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.Exec("CREATE TABLE turns(id TEXT); INSERT INTO turns VALUES('t')"); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Remember(context.Background(), db, "m", "project", "original", "manual", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO memory_imports VALUES(?,'trace-import','turns','t','f','m')", source); err != nil {
		t.Fatal(err)
	}
	db.Close()
	run := func(args ...string) []byte {
		t.Helper()
		var out bytes.Buffer
		cmd := newRoot()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--db", path}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %s %v", args, &out, err)
		}
		return out.Bytes()
	}
	run("update", "m", "revised")
	var got store.Memory
	if err := json.Unmarshal(run("get", "m"), &got); err != nil || got.Content != "revised" {
		t.Fatalf("get: %+v %v", got, err)
	}
	var history struct {
		Revisions []store.Revision `json:"revisions"`
	}
	if err := json.Unmarshal(run("history", "m"), &history); err != nil || len(history.Revisions) != 2 {
		t.Fatalf("history: %+v %v", history, err)
	}
	var verified verification
	if err := json.Unmarshal(run("verify", "m"), &verified); err != nil || verified.Status != "references_present" || verified.ContentVerified {
		t.Fatalf("false verification: %+v %v", verified, err)
	}
	if _, err := src.Exec("DELETE FROM turns"); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(run("verify", "m"), &verified); err != nil || verified.Status != "unverified_references" || verified.References[0].Status != "missing" {
		t.Fatalf("missing reference accepted: %+v %v", verified, err)
	}
	run("forget", "m")
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM memories").Scan(&count); err != nil || count != 0 {
		t.Fatalf("forget: %d %v", count, err)
	}
}
func TestMemoryReadsDoNotCreateDatabase(t *testing.T) {
	for _, name := range []string{"get", "history", "verify"} {
		path := filepath.Join(t.TempDir(), "missing.db")
		cmd := newRoot()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--db", path, name, "m"})
		if err := cmd.Execute(); err == nil {
			t.Fatal("missing index accepted")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("read created an index")
		}
	}
}
