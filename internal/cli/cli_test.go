package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/UseTrace-SI/trace-brain/internal/store"
)

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
