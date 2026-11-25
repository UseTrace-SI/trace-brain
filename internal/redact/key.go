package redact

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The marker key is shared with the Trace CLI (trace-cli internal/redact):
// the same file, the same HMAC derivation and the same creation rules, so
// every Trace component labels an equal secret with the same marker and none
// replaces a key another component published. Keep this file in step with
// trace-cli's internal/redact/key.go and internal/userstate.Path.

// markerKeyFile holds the per-install key that makes redaction markers
// unguessable. A marker built from an unsalted hash of the secret lets anyone
// holding the history confirm a guessed secret, or brute-force a short one.
const markerKeyFile = "redaction.key"

var markerKey struct {
	once       sync.Once
	key        []byte
	persistent bool
}

// marker labels a redacted value. Its 8 hex digits are a keyed digest of the
// value, so equal secrets carry equal markers within one installation.
func marker(kind, value string) string {
	mac := hmac.New(sha256.New, currentMarkerKey())
	mac.Write([]byte(value))
	return "⟨redacted:" + kind + ":" + hex.EncodeToString(mac.Sum(nil))[:8] + "⟩"
}

// markerKeyPath mirrors trace-cli's userstate.Path: $TRACE_HOME (absolute)
// or ~/.trace.
func markerKeyPath() (string, error) {
	if directory := os.Getenv("TRACE_HOME"); directory != "" {
		if !filepath.IsAbs(directory) {
			return "", fmt.Errorf("TRACE_HOME must be an absolute directory")
		}
		return filepath.Join(directory, markerKeyFile), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".trace", markerKeyFile), nil
}

func currentMarkerKey() []byte {
	markerKey.once.Do(func() {
		// Tests must not write into the real home directory; a per-process key
		// keeps their markers stable within a run.
		if !testing.Testing() {
			if path, err := markerKeyPath(); err == nil {
				if key, err := loadMarkerKey(path); err == nil {
					markerKey.key, markerKey.persistent = key, true
					return
				}
			}
		}
		// Without a readable state directory, redaction must still work: an
		// ephemeral key keeps markers unguessable, though not stable across
		// processes. Fingerprints never depend on marker digests (see
		// Canonical), so this does not make stored fingerprints stale.
		markerKey.key = randomMarkerKey()
	})
	return markerKey.key
}

// PersistentMarkerKey reports whether this process labels secrets with the
// shared per-install key (true) or an ephemeral per-process key (false).
func PersistentMarkerKey() bool {
	currentMarkerKey()
	return markerKey.persistent
}

func randomMarkerKey() []byte {
	key := make([]byte, 32)
	_, _ = rand.Read(key) // crypto/rand.Read never fails on supported platforms.
	return key
}

// loadMarkerKey reads the key at path, creating it with mode 0600 when it is
// missing or damaged. Concurrent first runs agree on one key: a new key is
// published with a hard link, which never replaces a key another process
// published first. Creation and repair are serialized by an advisory lock on
// path+".lock": inside it the key is read again, and only a key that is still
// damaged is moved aside (to a unique path+".damaged-<unixnano>") before a new
// one is linked in its place. If the lock cannot be taken within a few
// seconds, the same steps run unlocked, as before the lock existed.
func loadMarkerKey(path string) ([]byte, error) {
	key, err := readMarkerKey(path)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errDamagedMarkerKey) {
		return nil, err
	}
	_ = os.MkdirAll(filepath.Dir(path), 0700) // publishMarkerKey reports failures.
	if unlock, lockErr := lockMarkerKey(path + ".lock"); lockErr == nil {
		defer unlock()
		// Another process may have created or repaired the key meanwhile.
		key, err = readMarkerKey(path)
		if err == nil {
			return key, nil
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errDamagedMarkerKey) {
			return nil, err
		}
	}
	if errors.Is(err, errDamagedMarkerKey) {
		// Keep the damaged file for inspection; never delete or overwrite a
		// key in place, so a key published meanwhile is never lost.
		aside := fmt.Sprintf("%s.damaged-%d", path, time.Now().UnixNano())
		if err := os.Rename(path, aside); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("moving damaged marker key aside: %w", err)
		}
	}
	return publishMarkerKey(path)
}

// publishMarkerKey writes a new random key to a private temporary file and
// hard-links it to path. If a key appeared there first, that key is used.
func publishMarkerKey(path string) ([]byte, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("creating state directory: %w", err)
	}
	key := randomMarkerKey()
	tmp, err := os.CreateTemp(dir, markerKeyFile+".*.tmp")
	if err != nil {
		return nil, fmt.Errorf("creating marker key: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("restricting marker key: %w", err)
	}
	if _, err := tmp.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("writing marker key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("syncing marker key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("closing marker key: %w", err)
	}
	if err := os.Link(name, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return readMarkerKey(path)
		}
		return nil, fmt.Errorf("publishing marker key: %w", err)
	}
	return key, nil
}

// markerKeyLockWait bounds how long a process waits for another to finish
// creating or repairing the key before it proceeds without the lock.
var markerKeyLockWait = 5 * time.Second

var errDamagedMarkerKey = errors.New("marker key is damaged")

func readMarkerKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("marker key %s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info.Mode().Perm()&0077 != 0 {
		_ = f.Chmod(0600)
	}
	data, err := io.ReadAll(io.LimitReader(f, 256))
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) != 32 {
		return nil, errDamagedMarkerKey
	}
	return key, nil
}

// CheckMarkerKey reports whether the per-install marker key exists as a
// private regular file this process can read. When it cannot be used, every
// process redacts with its own ephemeral key, so equal secrets get different
// markers. It never creates or repairs the key.
func CheckMarkerKey() (string, error) {
	path, err := markerKeyPath()
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, fmt.Errorf("%s is missing; run trace init or start the daemon to create it", path)
	}
	if err != nil {
		return path, err
	}
	if !info.Mode().IsRegular() {
		return path, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Mode().Perm()&0077 != 0 {
		return path, fmt.Errorf("%s has mode %04o; want 0600", path, info.Mode().Perm())
	}
	f, err := os.Open(path)
	if err != nil {
		return path, fmt.Errorf("%s is not readable: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 256))
	if err != nil {
		return path, fmt.Errorf("reading %s: %w", path, err)
	}
	if key, err := hex.DecodeString(strings.TrimSpace(string(data))); err != nil || len(key) != 32 {
		return path, fmt.Errorf("%s is damaged", path)
	}
	return path, nil
}
