package instance

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// An orphan's immediate shutdown takes milliseconds; the bound only keeps a stuck process from
// blocking the start.
const (
	orphanExitTimeout = 10 * time.Second
	orphanExitPoll    = 10 * time.Millisecond
)

// claim returns a server directory this process holds the lock of: the stable one when it is
// free, else a new one. A process holds its lock until it exits, so a directory whose lock can be
// taken belongs to nobody, and a server still running in it is an orphan.
func claim(state, stable string) (*Instance, error) {
	if stable != "" {
		// held by another process or not usable: a new directory still serves, with another URL
		if instance, err := claimStable(filepath.Join(state, stablePrefix+stable)); err == nil && instance != nil {
			return instance, nil
		}
	}
	return claimNew(state)
}

// claimStable returns nil without an error when another process holds the directory. Nothing
// removes a stable directory or its lock file (release and Sweep only empty it), so the lock of
// the file is the lock of the directory.
func claimStable(directory string) (*Instance, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create the server directory: %w", err)
	}
	lock, acquired, err := lockFile(filepath.Join(directory, lockName), true)
	if err != nil || !acquired {
		return nil, err // nil, nil: another process holds it
	}
	stopOrphan(directory)
	if err := clearDirectory(directory); err != nil {
		return nil, errors.Join(fmt.Errorf("clear the server directory: %w", err), lock.Close())
	}
	return &Instance{directory: directory, stable: true, lock: lock}, nil
}

// claimNew creates the directory under a name the sweep ignores, takes its lock and only then
// renames it into the swept names: the sweep can never see an unlocked directory of a live
// process.
func claimNew(state string) (*Instance, error) {
	creating, err := os.MkdirTemp(state, creatingName)
	if err != nil {
		return nil, fmt.Errorf("create the server directory: %w", err)
	}
	lock, _, err := lockFile(filepath.Join(creating, lockName), true)
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(creating))
	}
	directory := filepath.Join(state, uniquePrefix+strings.TrimPrefix(filepath.Base(creating), creatingName))
	if err := os.Rename(creating, directory); err != nil {
		return nil, errors.Join(fmt.Errorf("create the server directory: %w", err), lock.Close(), os.RemoveAll(creating))
	}
	return &Instance{directory: directory, lock: lock}, nil
}

// Sweep stops the servers of processes that died without stopping them (a killed test binary, a
// test timeout) and removes their files: the whole directory of a new one, everything but the
// lock of a stable one (see claimStable). It skips every directory whose lock a live process
// holds, so the servers of other test runs and other checkouts stay untouched. A new directory's
// name is never reused, so a sweeper that takes the lock of one another sweeper removed meanwhile
// removes nothing of anybody's.
//
// shortcut: a process that dies between creating a directory and renaming it leaves an empty
// .new- directory behind; sweep them by age if they ever pile up.
func Sweep(state string) {
	entries, err := os.ReadDir(state)
	if err != nil {
		return // nothing to sweep yet
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !strings.HasPrefix(name, stablePrefix) && !strings.HasPrefix(name, uniquePrefix) {
			continue
		}
		directory := filepath.Join(state, name)
		lock, acquired, err := lockFile(filepath.Join(directory, lockName), false)
		if err != nil || !acquired {
			continue // in use, or no lock file: not one of ours
		}
		stopOrphan(directory)
		if strings.HasPrefix(name, stablePrefix) {
			_ = clearDirectory(directory) // a leftover is harmless and swept again next time
		} else {
			_ = os.RemoveAll(directory) // a leftover is harmless and swept again next time
		}
		_ = lock.Close() // closing releases the lock; nothing was written to it
	}
}

// stopOrphan stops the server of directory, if one still runs, with the immediate-shutdown signal
// and waits for it to exit, so a new server can take over the directory's socket.
func stopOrphan(directory string) {
	data := filepath.Join(directory, dataName)
	content, err := os.ReadFile(filepath.Join(data, "postmaster.pid")) //nolint:gosec // G304: a file of a server directory of this package
	if err != nil {
		return
	}
	processID, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(string(content), "\n", 2)[0]))
	if err != nil || !runsFrom(processID, data) {
		return
	}
	process, err := os.FindProcess(processID)
	if err != nil || process.Signal(syscall.SIGQUIT) != nil {
		return // gone already
	}
	for deadline := time.Now().Add(orphanExitTimeout); time.Now().Before(deadline); time.Sleep(orphanExitPoll) {
		if process.Signal(syscall.Signal(0)) != nil {
			return
		}
	}
}

// runsFrom reports whether the process runs with the data directory on its command line: after a
// server died, its process ID may belong to an unrelated process.
func runsFrom(processID int, data string) bool {
	command, err := exec.Command("ps", "-ww", "-o", "command=", "-p", strconv.Itoa(processID)).Output() //nolint:gosec,noctx // G204: a process ID read from a server directory; ps returns at once
	return err == nil && strings.Contains(string(command), data)
}

// clearDirectory removes everything in directory but its lock.
func clearDirectory(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if entry.Name() != lockName {
			errs = append(errs, os.RemoveAll(filepath.Join(directory, entry.Name())))
		}
	}
	return errors.Join(errs...)
}
