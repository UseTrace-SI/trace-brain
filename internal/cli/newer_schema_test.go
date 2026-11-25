package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-brain/internal/store"
)

// Read-only commands never migrate, so they must refuse a database written by
// a newer release rather than report it as ready.
func TestReadOnlyCommandsRefuseNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Remember(context.Background(), db, "m", "project", "deploy notes", "manual", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", store.SchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		cmd := newRoot()
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"--db", path}, args...))
		err := cmd.Execute()
		return out.String(), err
	}
	for _, args := range [][]string{
		{"context", "--trace-db", "", "--query", "deploy"},
		{"context", "--trace-db", ""},
		{"get", "m"},
		{"history", "m"},
		{"facts", "query"},
		{"recall", "deploy"},
	} {
		out, err := run(args...)
		if !errors.Is(err, store.ErrNewerSchema) {
			t.Fatalf("%v accepted a newer schema: %q %v", args, out, err)
		}
		if strings.Contains(out, `"ready"`) {
			t.Fatalf("%v reported ready: %q", args, out)
		}
	}
	out, err := run("workspace", "query", "deploy", "--databases", path)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Partial bool              `json:"partial"`
		Sources []workspaceSource `json:"sources"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil || !report.Partial || report.Sources[0].Status != "unavailable" {
		t.Fatalf("workspace read a newer schema: %s %v", out, err)
	}
}
