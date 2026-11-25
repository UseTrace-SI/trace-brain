package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// The reported misses: `recall test` did not find "tests are flaky",
// `recall decide` did not find "We decided…", and `recall "the database"`
// returned "the cat sat".
func TestRecallFindsStemmedWordsAndIgnoresStopwords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
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
	run("remember", "tests are flaky")
	run("remember", "We decided to use SQLite for local storage")
	run("remember", "the cat sat on the mat")
	run("remember", "the database runs in WAL mode")
	if out := run("recall", "test"); !strings.Contains(out, "tests are flaky") {
		t.Fatalf("recall test: %q", out)
	}
	if out := run("recall", "decide"); !strings.Contains(out, "We decided") {
		t.Fatalf("recall decide: %q", out)
	}
	if out := run("recall", "the database"); strings.Contains(out, "cat sat") || !strings.Contains(out, "WAL mode") {
		t.Fatalf("recall \"the database\": %q", out)
	}
	if out := run("query", `"the" OR database*`); strings.Contains(out, "cat sat") || !strings.Contains(out, "WAL mode") {
		t.Fatalf("operators were not literal: %q", out)
	}
}
