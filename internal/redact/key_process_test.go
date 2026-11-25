package redact

import (
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMarkerKeyHelperProcess is run as a separate process by the tests
// below: it waits for the start signal, loads the key and prints it.
func TestMarkerKeyHelperProcess(t *testing.T) {
	path := os.Getenv("TRACE_BRAIN_KEY_HELPER")
	if path == "" {
		t.Skip("helper process only")
	}
	start := path + ".start"
	for {
		if _, err := os.Stat(start); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	key, err := loadMarkerKey(path)
	if err != nil {
		fmt.Println("error", err)
		os.Exit(0)
	}
	fmt.Println("key", hex.EncodeToString(key))
	os.Exit(0)
}

// Separate processes that find the key missing or damaged at the same time
// must all end up with the one key that stays on disk.
func TestConcurrentProcessesAgreeOnOneKey(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	for _, damaged := range []bool{false, true} {
		for round := 0; round < 10; round++ {
			dir := t.TempDir()
			path := filepath.Join(dir, markerKeyFile)
			if damaged {
				if err := os.WriteFile(path, []byte("not a key"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			outputs := make([]string, 8)
			var wg sync.WaitGroup
			commands := make([]*exec.Cmd, len(outputs))
			for i := range commands {
				cmd := exec.Command(os.Args[0], "-test.run=^TestMarkerKeyHelperProcess$", "-test.count=1")
				cmd.Env = append(os.Environ(), "TRACE_BRAIN_KEY_HELPER="+path)
				commands[i] = cmd
			}
			for i, cmd := range commands {
				wg.Add(1)
				go func() {
					defer wg.Done()
					out, err := cmd.Output()
					if err != nil {
						t.Errorf("helper: %v", err)
					}
					outputs[i] = strings.TrimSpace(string(out))
				}()
			}
			// Let the helpers start polling, then release them together.
			time.Sleep(200 * time.Millisecond)
			if err := os.WriteFile(path+".start", nil, 0600); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			onDisk, err := readMarkerKey(path)
			if err != nil {
				t.Fatalf("damaged=%v: key on disk unusable: %v", damaged, err)
			}
			want := "key " + hex.EncodeToString(onDisk)
			for i, got := range outputs {
				if !strings.HasPrefix(got, want) {
					t.Fatalf("damaged=%v round %d: process %d got %q, disk has %q", damaged, round, i, got, want)
				}
			}
		}
	}
}

func TestDamagedKeyIsMovedAsideUnderAPrivateLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, markerKeyFile)
	// Two damages in a row each keep their own copy aside.
	for _, damaged := range []string{"first damage", "second damage"} {
		if err := os.WriteFile(path, []byte(damaged), 0600); err != nil {
			t.Fatal(err)
		}
		key, err := loadMarkerKey(path)
		if err != nil || len(key) != 32 {
			t.Fatalf("repair: %x %v", key, err)
		}
	}
	asides, err := filepath.Glob(path + ".damaged-*")
	if err != nil || len(asides) != 2 {
		t.Fatalf("damaged keys not kept aside separately: %v %v", asides, err)
	}
	kept := map[string]bool{}
	for _, aside := range asides {
		data, err := os.ReadFile(aside)
		if err != nil {
			t.Fatal(err)
		}
		kept[string(data)] = true
	}
	if !kept["first damage"] || !kept["second damage"] {
		t.Fatalf("a damaged copy was overwritten: %v", kept)
	}
	if info, err := os.Lstat(path + ".lock"); err != nil || info.Mode().Perm() != 0600 || !info.Mode().IsRegular() {
		t.Fatalf("lock file: %v %v", info, err)
	}
	// A symlinked lock file is never followed; the key is still created,
	// without the lock.
	other := filepath.Join(t.TempDir(), markerKeyFile)
	if err := os.Symlink(filepath.Join(dir, "elsewhere"), other+".lock"); err != nil {
		t.Fatal(err)
	}
	if key, err := loadMarkerKey(other); err != nil || len(key) != 32 {
		t.Fatalf("broken lock path blocked key creation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "elsewhere")); !os.IsNotExist(err) {
		t.Fatal("created the symlink target")
	}
}

// A lock held for too long never blocks key creation indefinitely.
func TestMarkerKeyLockWaitIsBounded(t *testing.T) {
	original := markerKeyLockWait
	markerKeyLockWait = 50 * time.Millisecond
	defer func() { markerKeyLockWait = original }()
	path := filepath.Join(t.TempDir(), markerKeyFile)
	unlock, err := lockMarkerKey(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	start := time.Now()
	if key, err := loadMarkerKey(path); err != nil || len(key) != 32 {
		t.Fatalf("held lock blocked creation: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waited %s for a held lock", elapsed)
	}
}
