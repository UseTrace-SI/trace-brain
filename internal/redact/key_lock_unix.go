//go:build unix

package redact

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// lockMarkerKey takes an exclusive advisory lock on the lock file at path,
// creating it with mode 0600 without following a symlink, and returns the
// function that releases it. It never blocks for longer than
// markerKeyLockWait; callers fall back to unlocked creation on error.
func lockMarkerKey(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(markerKeyLockWait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EINTR {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, errors.New("timed out waiting for the marker key lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
