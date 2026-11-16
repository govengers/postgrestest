//go:build unix

package instance

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockFile takes the exclusive lock of the file at path without waiting. It reports false when
// another process, or another open file of this one, holds it, and when the file is missing and
// create is false. The lock lasts until the returned file is closed or the process exits; child
// processes do not inherit it, so an orphaned server never holds the lock of its directory.
func lockFile(path string, create bool) (*os.File, bool, error) {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	file, err := os.OpenFile(path, flags, 0o600) //nolint:gosec // G304: a lock file of the state directory
	if errors.Is(err, os.ErrNotExist) && !create {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open the lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close() // only the lock mattered
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("lock %s: %w", path, err)
	}
	return file, true, nil
}

// waitForLock takes the exclusive lock of the file at path, creating it, and waits for it.
func waitForLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: a lock file of the state directory
	if err != nil {
		return nil, fmt.Errorf("open the lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close() // only the lock mattered
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return file, nil
}

// privateDirectory fails unless path is a directory, not a link, of the current user that only
// that user can enter.
func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ok || int(owner.Uid) != os.Getuid() {
		return errors.New("not a private directory of the current user")
	}
	return nil
}
