//go:build e2e

// Package e2e runs postgrestest the way consumers do: go test on the fixture packages, on their own
// and through the launcher binary in E2E_BINARY, and checks the server lifecycle around them.
package e2e

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/govengers/postgrestest/internal/instance"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

const (
	fixtures      = "./test/e2e/fixture/..."
	fixtureTag    = "e2efixture"
	waitForServer = 30 * time.Second
	pollInterval  = 20 * time.Millisecond
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestPackageServersStartPerTestBinaryAndHitTheCache(t *testing.T) {
	first := goTest(t, nil)
	second := goTest(t, nil)

	directories := dataDirectories(t, first)
	require.Len(t, directories, 2)
	require.NotEqual(t, directories[0], directories[1], "every test binary has a server of its own")
	require.Equal(t, 2, strings.Count(second, "(cached)"), second)
	for _, directory := range directories {
		require.NoDirExists(t, filepath.Dir(directory), "Main removed the server directory")
	}
}

func TestLauncherSharesOneServerAndHitsTheCache(t *testing.T) {
	first := goTest(t, []string{launcher(t)})
	second := goTest(t, []string{launcher(t)})

	directories := dataDirectories(t, first)
	require.Len(t, directories, 2)
	require.Equal(t, directories[0], directories[1], "both test binaries attached to the launcher's server")
	require.Equal(t, 2, strings.Count(second, "(cached)"), "the URL is the same in every run: %s", second)
	require.NoDirExists(t, directories[0], "the launcher stopped its server")
}

func TestLauncherExitsWithTheStatusOfTheCommand(t *testing.T) {
	directory := t.TempDir()
	require.Equal(t, 7, exitCode(t, runLauncher(t, directory, nil, "sh", "-c", `test -n "$POSTGRESTEST_URL" && exit 7`)))
	require.Equal(t, 0, exitCode(t, runLauncher(t, directory, nil, "--", "true")))
	require.Equal(t, 2, exitCode(t, runLauncher(t, directory, nil)), "no command")
	require.Equal(t, 1, exitCode(t, runLauncher(t, directory, nil, "/nonexistent/command")))
}

func TestLauncherUsesAGivenURLAsIs(t *testing.T) {
	given := "postgres://postgres@/postgres?host=/nonexistent"
	output, err := launcherCommand(t, t.TempDir(), []string{"POSTGRESTEST_URL=" + given}, "sh", "-c", `echo "$POSTGRESTEST_URL"`).Output()

	require.NoError(t, err)
	require.Equal(t, given, strings.TrimSpace(string(output)))
}

func TestLauncherStopsTheServerOfAKilledRun(t *testing.T) {
	directory := t.TempDir()
	killed := startBlockingLauncher(t, directory)
	postmaster := postmasterOf(t, killed.socketDirectory)
	require.NoError(t, killed.command.Process.Kill())
	_ = killed.command.Wait() // killed on purpose; Wait also closes the input, which ends the orphaned shell
	require.True(t, alive(postmaster), "a killed launcher leaves its server running")

	require.Equal(t, 0, exitCode(t, runLauncher(t, directory, nil, "true")))

	require.Eventually(t, func() bool { return !alive(postmaster) }, waitForServer, pollInterval, "the next run stopped the orphan")
}

func TestLauncherLeavesTheServerOfAnotherCheckoutRunning(t *testing.T) {
	running := startBlockingLauncher(t, t.TempDir())

	require.Equal(t, 0, exitCode(t, runLauncher(t, t.TempDir(), nil, "true")))

	connection, err := pgx.Connect(t.Context(), running.url)
	require.NoError(t, err, "the other checkout's server still accepts connections")
	require.NoError(t, connection.Close(t.Context()))
	require.NoError(t, running.input.Close())
	require.NoError(t, running.command.Wait(), "the launcher stops its server when its command ends")
	require.NoDirExists(t, filepath.Join(running.socketDirectory, "data"))
}

// goTest runs go test on the fixtures from the module root, through the given launcher command,
// with an environment that neither disables the test cache nor names a server.
func goTest(t *testing.T, through []string) string {
	t.Helper()
	arguments := slices.Concat(through, []string{"go", "test", "-v", "-tags", fixtureTag, fixtures})
	command := exec.CommandContext(t.Context(), arguments[0], arguments[1:]...)
	command.Dir = moduleRoot(t)
	command.Env = environment(nil)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	return string(output)
}

var dataDirectoryLine = regexp.MustCompile(`data_directory=(\S+)`)

func dataDirectories(t *testing.T, output string) []string {
	t.Helper()
	var directories []string
	for _, match := range dataDirectoryLine.FindAllStringSubmatch(output, -1) {
		directories = append(directories, match[1])
	}
	return directories
}

type blockingLauncher struct {
	command         *exec.Cmd
	input           io.WriteCloser
	url             string
	socketDirectory string
}

// startBlockingLauncher starts the launcher in directory with a command that prints the URL and
// waits until its input closes, then succeeds.
func startBlockingLauncher(t *testing.T, directory string) *blockingLauncher {
	t.Helper()
	command := launcherCommand(t, directory, nil, "sh", "-c", `echo "$POSTGRESTEST_URL"; read line || :`)
	input, err := command.StdinPipe()
	require.NoError(t, err)
	output, err := command.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, command.Start())
	t.Cleanup(func() {
		_ = input.Close()          // ends the command if the test did not
		_ = command.Process.Kill() // a launcher that is still running after the test
		_, _ = io.Copy(io.Discard, output)
		_ = command.Wait() // the test checked the status it cared about
	})
	line, err := bufio.NewReader(output).ReadString('\n')
	require.NoError(t, err)
	address, err := url.Parse(strings.TrimSpace(line))
	require.NoError(t, err)
	return &blockingLauncher{command: command, input: input, url: address.String(), socketDirectory: address.Query().Get("host")}
}

func postmasterOf(t *testing.T, serverDirectory string) int {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(serverDirectory, "data", "postmaster.pid")) //nolint:gosec // G304: the directory of the launcher under test
	require.NoError(t, err)
	processID, err := strconv.Atoi(strings.SplitN(string(content), "\n", 2)[0])
	require.NoError(t, err)
	return processID
}

func alive(processID int) bool { return syscall.Kill(processID, 0) == nil }

func runLauncher(t *testing.T, directory string, extra []string, arguments ...string) error {
	t.Helper()
	command := launcherCommand(t, directory, extra, arguments...)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	return command.Run()
}

func launcherCommand(t *testing.T, directory string, extra []string, arguments ...string) *exec.Cmd {
	t.Helper()
	command := exec.CommandContext(context.Background(), launcher(t), arguments...)
	command.Dir = directory
	command.Env = environment(extra)
	removeStableDirectory(t, directory)
	return command
}

// removeStableDirectory removes, after the test and its launchers, the run- directory the launcher
// names after directory: nothing else does (P9), and a temporary checkout never runs again.
func removeStableDirectory(t *testing.T, directory string) {
	t.Helper()
	state, err := instance.DefaultStateDirectory()
	require.NoError(t, err)
	physical, err := filepath.EvalSymlinks(directory) // the launcher names the directory the kernel reports
	require.NoError(t, err)
	t.Cleanup(func() {
		digest := sha256.Sum256([]byte(physical))
		require.NoError(t, os.RemoveAll(filepath.Join(state, "run-"+hex.EncodeToString(digest[:8]))))
	})
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	require.ErrorAs(t, err, &exitError)
	return exitError.ExitCode()
}

func launcher(t *testing.T) string {
	t.Helper()
	binary := os.Getenv("E2E_BINARY")
	require.NotEmpty(t, binary, "E2E_BINARY names the launcher binary (just test-e2e builds it)")
	return binary
}

// environment is the suite's own without the variables that would change what the fixtures test.
func environment(extra []string) []string {
	kept := slices.DeleteFunc(os.Environ(), func(variable string) bool {
		name, _, _ := strings.Cut(variable, "=")
		return name == "POSTGRESTEST_URL" || name == "GOFLAGS"
	})
	return append(kept, extra...)
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	directory, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	return directory
}
