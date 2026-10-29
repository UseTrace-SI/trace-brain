package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/UseTrace-SI/trace-brain/internal/store"
)

func TestCLIRememberRecall(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "m.db")
	old := dbPath
	dbPath = db
	defer func() { dbPath = old }()
	root := newRoot()
	root.SetArgs([]string{"--db", db, "remember", "retry failed requests"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	// verify via store
	sdb, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	var n int
	if err := sdb.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n); err != nil || n != 1 {
		t.Fatalf("count=%d err=%v", n, err)
	}
	_ = os.Remove
}
