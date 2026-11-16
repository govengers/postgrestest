//go:build unix

package instance

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// The one embedded server of this package's tests: a stable directory, as the launcher uses.
func TestStableServerKeepsItsURLAndReleasesItsDirectory(t *testing.T) {
	state, err := DefaultStateDirectory()
	require.NoError(t, err)
	stable := "instance-test-" + strconv.Itoa(os.Getpid())
	directory := filepath.Join(state, stablePrefix+stable)
	t.Cleanup(func() { _ = os.RemoveAll(directory) }) // a unique name per run; nothing else uses it

	server, err := Start(t.Context(), &Options{Stable: stable})
	require.NoError(t, err)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = server.Stop() // the assertions failed; the stop error adds nothing
		}
	})

	require.Equal(t, directory, server.Directory())
	require.Equal(t, "postgres://postgres@/postgres?host="+url.QueryEscape(directory)+"&sslmode=disable", server.URL())
	connection, err := pgx.Connect(t.Context(), server.URL())
	require.NoError(t, err)
	var fsync string
	require.NoError(t, connection.QueryRow(t.Context(), "SHOW fsync").Scan(&fsync))
	require.NoError(t, connection.Close(t.Context()))
	require.Equal(t, "off", fsync)

	held, acquired, err := lockFile(filepath.Join(directory, lockName), false)
	require.NoError(t, err)
	require.False(t, acquired, "the running server holds its directory")
	require.Nil(t, held)

	stopped = true
	require.NoError(t, server.Stop())
	require.NoDirExists(t, directory)
}

func TestHeldStableDirectoryFallsBackToANewOne(t *testing.T) {
	state := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(state, stablePrefix+"busy"), 0o700))
	held, acquired, err := lockFile(filepath.Join(state, stablePrefix+"busy", lockName), true)
	require.NoError(t, err)
	require.True(t, acquired)
	t.Cleanup(func() { require.NoError(t, held.Close()) })

	claimed, err := claim(state, "busy")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, claimed.release()) })

	require.Regexp(t, `/i-[0-9]+$`, claimed.directory)
}

func TestClaimClearsAStableDirectoryOfADeadProcess(t *testing.T) {
	state := t.TempDir()
	directory := filepath.Join(state, stablePrefix+"left")
	writeFile(t, filepath.Join(directory, dataName, "PG_VERSION"))
	writeFile(t, filepath.Join(directory, lockName))

	claimed, err := claim(state, "left")
	require.NoError(t, err)

	require.Equal(t, directory, claimed.directory)
	require.NoDirExists(t, filepath.Join(directory, dataName))
	require.NoError(t, claimed.release())
	require.NoDirExists(t, directory)
}

// A sweep or a stop removes a directory while holding its lock; a claimer that opened the lock
// file before then holds the lock of a file nobody else can find.
func TestLockOfARemovedFileIsNoClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), lockName)
	lock, acquired, err := lockFile(path, true)
	require.NoError(t, err)
	require.True(t, acquired)
	t.Cleanup(func() { require.NoError(t, lock.Close()) })
	require.True(t, stillLinked(lock, path))

	require.NoError(t, os.Remove(path))
	require.False(t, stillLinked(lock, path))
	writeFile(t, path)
	require.False(t, stillLinked(lock, path), "a new file under the same name is another lock")
}

func TestSweepRemovesOnlyTheDirectoriesOfDeadProcesses(t *testing.T) {
	state := t.TempDir()
	for _, name := range []string{"i-dead", "i-live", "run-dead", "i-foreign", "postgres-18.3.0"} {
		writeFile(t, filepath.Join(state, name, dataName, "PG_VERSION"))
	}
	for _, name := range []string{"i-dead", "i-live", "run-dead", "postgres-18.3.0"} {
		writeFile(t, filepath.Join(state, name, lockName))
	}
	writeFile(t, filepath.Join(state, "i-file"))
	live, acquired, err := lockFile(filepath.Join(state, "i-live", lockName), false)
	require.NoError(t, err)
	require.True(t, acquired)
	t.Cleanup(func() { require.NoError(t, live.Close()) })

	Sweep(state)

	require.NoDirExists(t, filepath.Join(state, "i-dead"))
	require.DirExists(t, filepath.Join(state, "i-live", dataName), "a live process holds it")
	require.NoDirExists(t, filepath.Join(state, "run-dead"))
	require.DirExists(t, filepath.Join(state, "i-foreign", dataName), "no lock: not a server directory")
	require.DirExists(t, filepath.Join(state, "postgres-18.3.0", dataName), "not a server name")
	require.FileExists(t, filepath.Join(state, "i-file"))
	Sweep(filepath.Join(state, "missing")) // nothing to sweep is fine
}

// The fake server has the data directory on its command line, as a postmaster started by pg_ctl;
// the unrelated process does not.
func TestStopOrphanSignalsOnlyTheServerOfItsDirectory(t *testing.T) {
	data := filepath.Join(t.TempDir(), dataName)
	unrelated := startProcess(t, "unrelated")
	writePostmasterPID(t, data, unrelated.Process.Pid)
	stopOrphan(filepath.Dir(data))
	require.False(t, runsFrom(unrelated.Process.Pid, data))

	server := startProcess(t, data)
	writePostmasterPID(t, data, server.Process.Pid)
	require.True(t, runsFrom(server.Process.Pid, data))
	exited := make(chan error, 1)
	go func() { exited <- server.Wait() }() // reaps it, as init reaps a real orphan: Linux still signals a zombie
	stopOrphan(filepath.Dir(data))

	select {
	case err := <-exited:
		require.ErrorContains(t, err, "signal: quit")
	case <-time.After(10 * time.Second):
		t.Fatal("the orphaned server was not stopped")
	}
}

// startProcess starts a shell that waits with argument on its command line, in a process group of
// its own that the cleanup kills.
func startProcess(t *testing.T, argument string) *exec.Cmd {
	t.Helper()
	process := exec.CommandContext(context.Background(), "sh", "-c", "sleep 600; :", "postgres", argument)
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, process.Start())
	t.Cleanup(func() { _ = syscall.Kill(-process.Process.Pid, syscall.SIGKILL) }) // gone already is fine
	return process
}

func writePostmasterPID(t *testing.T, data string, processID int) {
	t.Helper()
	require.NoError(t, os.MkdirAll(data, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(data, "postmaster.pid"), []byte(strconv.Itoa(processID)+"\n"+data+"\n"), 0o600))
}

func TestCompleteBinariesAreNotUnpackedAgain(t *testing.T) {
	state := t.TempDir()
	final := filepath.Join(state, binariesPrefix+string(Version))
	for _, program := range programs {
		writeFile(t, filepath.Join(final, "bin", program))
	}

	binaries, err := unpackedBinaries(t.Context(), state)

	require.NoError(t, err)
	require.Equal(t, final, binaries)
	require.NoFileExists(t, filepath.Join(state, unpackLockName), "the fast path takes no lock")
}

// A home directory whose embedded-postgres cache holds a small archive of empty programs: the
// unpack path runs in milliseconds and without a download.
func TestBinariesAreUnpackedOnceFromTheCachedArchive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	archive, err := archivePath()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(archive), 0o700))
	writeArchive(t, archive, "bin")
	state, err := DefaultStateDirectory()
	require.NoError(t, err)
	writeFile(t, filepath.Join(state, unpackingName+"killed", "bin", "initdb"))

	results := make(chan string, 2)
	for range 2 {
		go func() {
			binaries, err := unpackedBinaries(context.Background(), state)
			if err != nil {
				binaries = err.Error()
			}
			results <- binaries
		}()
	}

	final := filepath.Join(state, binariesPrefix+string(Version))
	require.Equal(t, final, <-results)
	require.Equal(t, final, <-results)
	require.True(t, complete(final))
	leftovers, err := filepath.Glob(filepath.Join(state, unpackingName+"*"))
	require.NoError(t, err)
	require.Empty(t, leftovers, "the scratch directories of this and of a killed unpack are gone")
}

// writeArchive packs the empty programs, or only the given members of bin, into an xz tarball.
func writeArchive(t *testing.T, archive string, members ...string) {
	t.Helper()
	source := t.TempDir()
	for _, program := range programs {
		writeFile(t, filepath.Join(source, "bin", program))
	}
	arguments := append([]string{"-cJf", archive, "-C", source}, members...)
	require.NoError(t, exec.CommandContext(t.Context(), "tar", arguments...).Run())
}

func TestUntarUnpacksACompleteArchiveOnly(t *testing.T) {
	archives := t.TempDir()
	complete := filepath.Join(archives, "complete.txz")
	writeArchive(t, complete, "bin")
	incomplete := filepath.Join(archives, "incomplete.txz")
	writeArchive(t, incomplete, "bin/initdb")
	target := filepath.Join(t.TempDir(), "binaries")

	require.Error(t, untar(t.Context(), filepath.Join(archives, "missing.txz"), target))
	require.ErrorContains(t, untar(t.Context(), incomplete, target), "lacks a PostgreSQL program")
	require.NoDirExists(t, target)
	require.NoError(t, untar(t.Context(), complete, target))
	require.FileExists(t, filepath.Join(target, "bin", "postgres"))
}

func TestArchivePlatformFollowsEmbeddedPostgres(t *testing.T) {
	for _, testCase := range []struct {
		operatingSystem, architecture string
		alpine                        bool
		expected                      string
	}{
		{"darwin", "arm64", false, "darwin-arm64v8"},
		{"darwin", "amd64", false, "darwin-amd64"},
		{"linux", "amd64", false, "linux-amd64"},
		{"linux", "arm64", false, "linux-arm64v8"},
		{"linux", "amd64", true, "linux-amd64-alpine"},
	} {
		platform, err := archivePlatform(testCase.operatingSystem, testCase.architecture, testCase.alpine)
		require.NoError(t, err)
		require.Equal(t, testCase.expected, platform)
	}
	_, err := archivePlatform("windows", "amd64", false)
	require.Error(t, err)
	path, err := archivePath()
	require.NoError(t, err)
	require.Contains(t, path, "embedded-postgres-binaries-")
}

func TestQuoteSetting(t *testing.T) {
	require.Equal(t, `'/it''s/here'`, quoteSetting("/it's/here"))
}

func TestFailedCommandReturnsTheLog(t *testing.T) {
	directory := t.TempDir()
	server := &Instance{directory: directory, binaries: directory}
	writeFile(t, filepath.Join(directory, "bin", "initdb"))

	err := server.command(t.Context(), "initdb", "-D", "nowhere")

	require.ErrorContains(t, err, "initdb -D")
	require.Contains(t, logTail(filepath.Join(t.TempDir(), "missing")), "no server log")
}

func TestTooLongSocketPathFailsTheStart(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "a-directory-name-that-makes-the-socket-path-longer-than-unix-allows-by-a-wide-margin")
	server := &Instance{directory: directory}

	require.ErrorContains(t, server.run(t.Context()), "longer than the 103 bytes")
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
}

func TestFailedStartRemovesTheServerDirectory(t *testing.T) {
	state, err := os.MkdirTemp("/tmp", "postgrestest-") //nolint:usetesting // t.TempDir is too long a path for a Unix socket on macOS
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(state)) })
	binaries := filepath.Join(state, binariesPrefix+string(Version), "bin")
	for _, program := range programs {
		require.NoError(t, os.MkdirAll(binaries, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(binaries, program), []byte("#!/bin/sh\necho "+program+" is broken\nexit 1\n"), 0o700)) //nolint:gosec // G306: a program the test runs
	}

	_, err = Start(t.Context(), &Options{StateDirectory: state})

	require.ErrorContains(t, err, "initdb is broken")
	directories, globError := filepath.Glob(filepath.Join(state, uniquePrefix+"*"))
	require.NoError(t, globError)
	require.Empty(t, directories)
}

func TestUnusableStateDirectoryFailsTheClaim(t *testing.T) {
	state := filepath.Join(t.TempDir(), "a-file")
	writeFile(t, state)

	_, stableError := claim(state, "x")
	_, newError := claim(state, "")
	_, lockError := waitForLock(filepath.Join(state, unpackLockName))
	_, acquired, missingError := lockFile(filepath.Join(t.TempDir(), "missing", lockName), true)

	require.ErrorContains(t, stableError, "create the server directory")
	require.ErrorContains(t, newError, "create the server directory")
	require.ErrorContains(t, lockError, "open the lock")
	require.ErrorIs(t, missingError, os.ErrNotExist)
	require.False(t, acquired)
}
