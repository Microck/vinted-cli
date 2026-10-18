// Package filelock supplies process locks for local session lifecycle and metadata.
package filelock

import (
	"context"
	"errors"
	"os"
	"time"
)

// Try opens a persistent lock file. Lock files must not be unlinked while in use:
// another process could otherwise lock a different inode at the same path.
func Try(path string) (*os.File, bool, error) {
	if st, err := os.Lstat(path); err == nil && !st.Mode().IsRegular() {
		return nil, false, errors.New("invalid lock file")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false, err
	}
	ok, err := tryLock(f)
	if !ok || err != nil {
		f.Close()
		return nil, ok, err
	}
	return f, true, nil
}

func Acquire(ctx context.Context, path string) (*os.File, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f, ok, err := Try(path)
		if err != nil || ok {
			return f, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func Release(f *os.File) error {
	if f == nil {
		return nil
	}
	err := unlock(f)
	return errors.Join(err, f.Close())
}
