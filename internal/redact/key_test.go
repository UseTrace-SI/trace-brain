package redact

// Ported from trace-cli internal/redact/key_test.go so both components keep
// identical key semantics.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// An unsalted digest of the secret would let anyone with the history confirm
// a guess; the marker must not be one.
func TestMarkerIsNotAnUnsaltedDigest(t *testing.T) {
	const secret = "hunter2hunter2"
	sum := sha256.Sum256([]byte(secret))
	m := marker("env", secret)
	if strings.Contains(m, hex.EncodeToString(sum[:])[:8]) {
		t.Fatalf("marker %q exposes the unsalted SHA-256 of the secret", m)
	}
	if !redactionMarker.MatchString(m) || m != marker("env", secret) {
		t.Fatalf("marker %q is not a stable, well-formed marker", m)
	}
	if marker("env", secret) == marker("env", secret+"!") {
		t.Fatal("different secrets share a marker")
	}
}

func TestMarkerKeyIsCreatedPrivatelyAndReused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", markerKeyFile)
	first, err := loadMarkerKey(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("key mode %v; want 0600", info.Mode().Perm())
	}
	if dir, err := os.Stat(filepath.Dir(path)); err != nil || dir.Mode().Perm() != 0700 {
		t.Fatalf("state directory mode %v, %v; want 0700", dir.Mode().Perm(), err)
	}
	second, err := loadMarkerKey(path)
	if err != nil || !bytes.Equal(first, second) || len(first) != 32 {
		t.Fatalf("key not reused across runs: %x %x %v", first, second, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary key files left behind: %v", entries)
	}
}

func TestMarkerKeyRepairsDamageAndTightensMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, markerKeyFile)
	if err := os.WriteFile(path, []byte("not a key"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := loadMarkerKey(path)
	if err != nil || len(key) != 32 {
		t.Fatalf("damaged key not replaced: %x %v", key, err)
	}
	if again, err := readMarkerKey(path); err != nil || !bytes.Equal(again, key) {
		t.Fatalf("replacement not saved: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMarkerKey(path); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatalf("readable key left at mode %v", info.Mode().Perm())
	}
}

func TestMarkerKeyRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte(strings.Repeat("ab", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, markerKeyFile)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMarkerKey(path); err == nil {
		t.Fatal("followed a symlinked marker key")
	}
}

func TestConcurrentFirstRunsAgreeOnOneKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), markerKeyFile)
	keys := make([][]byte, 16)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := loadMarkerKey(path)
			if err != nil {
				t.Error(err)
			}
			keys[i] = key
		}()
	}
	wg.Wait()
	for _, key := range keys[1:] {
		if !bytes.Equal(key, keys[0]) {
			t.Fatalf("concurrent runs created different keys: %x %x", keys[0], key)
		}
	}
}

func TestCheckMarkerKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TRACE_HOME", home)
	path := filepath.Join(home, markerKeyFile)
	if _, err := CheckMarkerKey(); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing key: %v", err)
	}
	if _, err := loadMarkerKey(path); err != nil {
		t.Fatal(err)
	}
	if got, err := CheckMarkerKey(); err != nil || got != path {
		t.Fatalf("usable key reported %q, %v", got, err)
	}
	for _, tc := range []struct {
		name, want string
		damage     func() error
	}{
		{"readable by others", "mode", func() error { return os.Chmod(path, 0644) }},
		{"unreadable", "not readable", func() error { return os.Chmod(path, 0) }},
		{"damaged", "damaged", func() error { return os.WriteFile(path, []byte("short"), 0600) }},
		{"symlink", "not a regular file", func() error {
			target := filepath.Join(t.TempDir(), "key")
			if err := os.WriteFile(target, []byte(strings.Repeat("ab", 32)), 0600); err != nil {
				return err
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Symlink(target, path)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "unreadable" && os.Geteuid() == 0 {
				t.Skip("root reads any file")
			}
			_ = os.Remove(path)
			if _, err := loadMarkerKey(path); err != nil {
				t.Fatal(err)
			}
			if err := tc.damage(); err != nil {
				t.Fatal(err)
			}
			if _, err := CheckMarkerKey(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CheckMarkerKey = %v; want an error mentioning %q", err, tc.want)
			}
			_ = os.Chmod(path, 0600)
		})
	}
}
