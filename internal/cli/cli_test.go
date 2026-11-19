package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-brain/internal/store"
)

// runtimeCredential builds a credential-shaped value at runtime so the source
// tree contains no literal that push protection would flag.
func runtimeCredential() string {
	const alphabet = "Ab3Cd5Ef7Gh9Jk1Lm2Np4Qr6"
	value := make([]byte, 40)
	for i := range value {
		value[i] = alphabet[i%len(alphabet)]
	}
	return string(value)
}

func TestRegressionCLIReadPathsReapplyRedaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	secret := runtimeCredential()
	// Direct insert emulates a legacy/corrupted row that escaped write-time
	// redaction. Read commands must re-apply the policy like get/viz do.
	if _, err := db.Exec("INSERT INTO memories(id,scope,content,source,created_at) VALUES('m',?,?,'manual',1)", "scope "+secret, "lexicalterm "+secret); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		cmd := newRoot()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--db", path}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.String()
	}
	for _, args := range [][]string{{"recall", "lexicalterm"}, {"query", "lexicalterm"}, {"brief", "lexicalterm"}, {"dash"}} {
		got := run(args...)
		if strings.Contains(got, secret) || !strings.Contains(got, "redacted:") {
			t.Fatalf("%v leaked unredacted stored text: %s", args, got)
		}
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "m.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("INSERT INTO memories(id,scope,content,source,created_at) VALUES('m1','s','hello world','manual',1)"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n); err != nil || n != 1 {
		t.Fatalf("count=%d err=%v", n, err)
	}
	_ = os.Remove
}
