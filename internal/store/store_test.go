package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenAndRemember(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.db")
	db, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("INSERT INTO memories(id,scope,content,source,created_at) VALUES('m1','s','hello','manual',1)"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n); err != nil || n != 1 {
		t.Fatalf("count=%d err=%v", n, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
}

