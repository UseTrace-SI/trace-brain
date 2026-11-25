//go:build unix

package redact

import (
	"os"
	"syscall"
)

// lockMarkerKey takes an exclusive advisory lock on the lock file at path,
// creating it with mode 0600 without following a symlink, and returns the
// function that releases it.
func lockMarkerKey(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
