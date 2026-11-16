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
)

// claim returns a server directory this process holds the lock of: the stable one when it is
// free, else a new one. A process holds its lock until it exits, so a directory whose lock can be
// taken belongs to nobody, and a server still running in it is an orphan.
func claim(state, stable string) (*Instance, error) {
	if stable != "" {
		instance, err := claimStable(filepath.Join(state, stablePrefix+stable))
		if instance != nil || err != nil {
			return instance, err
		}
	}
	return claimNew(state)
}

// claimStable returns nil without an error when another process holds the directory.
func claimStable(directory string) (*Instance, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create the server directory: %w", err)
	}
	lock, acquired, err := lockFile(filepath.Join(directory, lockName), true)
	if err != nil || !acquired {
		return nil, err
	}
	stopOrphan(directory)
	if err := clearDirectory(directory); err != nil {
		return nil, errors.Join(fmt.Errorf("clear the server directory: %w", err), lock.Close())
	}
	return &Instance{directory: directory, lock: lock, stable: true}, nil
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
// test timeout) and removes their files. It skips every directory whose lock a live process
// holds, so the servers of other test runs and other checkouts stay untouched.
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
		stable := strings.HasPrefix(name, stablePrefix)
		if !entry.IsDir() || !stable && !strings.HasPrefix(name, uniquePrefix) {
			continue
		}
		directory := filepath.Join(state, name)
		lock, acquired, err := lockFile(filepath.Join(directory, lockName), false)
		if err != nil || !acquired {
			continue // in use, or no lock file: not one of ours
		}
		stopOrphan(directory)
		if stable {
			_ = clearDirectory(directory) // the next claim of the directory clears it again
		} else {
			_ = os.RemoveAll(directory) // a leftover directory is harmless and swept again next time
		}
		_ = lock.Close() // closing releases the lock; nothing was written to it
	}
}

// stopOrphan sends the immediate-shutdown signal to the server of directory, if one still runs.
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
	if process, err := os.FindProcess(processID); err == nil {
		_ = process.Signal(syscall.SIGQUIT) // gone already is fine
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
