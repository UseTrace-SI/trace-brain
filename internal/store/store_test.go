package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

func TestConcurrentOpenAndRemember(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	start := make(chan struct{})
	errors := make(chan error, 8)
	var workers sync.WaitGroup
	for i := 0; i < cap(errors); i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			<-start
			db, err := Open(path)
			if err != nil {
				errors <- fmt.Errorf("open: %w", err)
				return
			}
			defer db.Close()
			errors <- Remember(context.Background(), db, fmt.Sprintf("memory-%d", i), "project", "concurrent memory", "manual", 1)
		}(i)
	}
	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Error(err)
		}
	}
}
