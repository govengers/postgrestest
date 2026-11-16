// Package instance runs the embedded PostgreSQL servers of postgrestest: the binaries unpacked once
// per machine, one directory per server below a state directory, settings tuned for tests, a Unix
// socket as the only address, and the sweep that stops the servers of test processes that died.
package instance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// Version is the PostgreSQL release every embedded server runs.
const Version = embeddedpostgres.V18

const (
	stateName     = "postgrestest"
	lockName      = "lock"
	dataName      = "data"
	logName       = "log"
	uniquePrefix  = "i-"
	stablePrefix  = "run-"
	creatingName  = ".new-"
	superuser     = "postgres"
	socketPort    = 5432
	socketName    = ".s.PGSQL.5432"
	startTimeout  = "60" // seconds pg_ctl waits for the server to accept connections
	logTailLength = 4096
	// shortSocketParent holds the socket of a server whose directory is too deep for one; not
	// os.TempDir, which is deep itself on macOS and would make TMPDIR a test cache input.
	shortSocketParent = "/tmp"
	shortSocketPrefix = "postgrestest-"
)

// settings tune the server for tests: nothing is flushed to disk (a crash loses the server, which
// is thrown away anyway), no TCP listener, enough connections for parallel tests of several
// packages on one server, no background work that makes timings vary, and UTC so results do not
// depend on the machine.
var settings = []string{
	"listen_addresses = ''",
	fmt.Sprintf("port = %d", socketPort),
	"max_connections = 500",
	"fsync = off",
	"synchronous_commit = off",
	"full_page_writes = off",
	"autovacuum = off",
	"jit = off",
	"timezone = 'UTC'",
	"log_timezone = 'UTC'",
}

// Options configure Start.
type Options struct {
	// StateDirectory holds the unpacked binaries and the server directories. Empty means
	// DefaultStateDirectory.
	StateDirectory string
	// Stable names a server directory that keeps its path, and so the server's URL, from run to
	// run. Empty, or a stable directory another live process holds, starts the server in a new
	// directory of its own.
	Stable string
}

// Instance is one running embedded server. Its directory holds the lock its process keeps for as
// long as the server runs, the data directory, the log and, unless its path is too long, the
// socket.
type Instance struct {
	directory string
	stable    bool   // a stable directory keeps its lock file, see release
	socket    string // the socket's directory, set by run
	binaries  string
	lock      *os.File
}

// DefaultStateDirectory is postgrestest below embedded-postgres' own archive cache,
// ~/.embedded-postgres-go, which the govengers CI restores, so a CI run finds the binaries
// unpacked. It is outside every module, so go test never hashes what it finds there.
func DefaultStateDirectory() (string, error) {
	cache, err := cacheDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, stateName), nil
}

// Start stops the servers of dead processes, unpacks the binaries when needed and starts a server
// in a directory this process holds until Stop.
func Start(ctx context.Context, options *Options) (*Instance, error) {
	state := options.StateDirectory
	if state == "" {
		var err error
		if state, err = DefaultStateDirectory(); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, fmt.Errorf("create the state directory: %w", err)
	}
	Sweep(state)
	binaries, err := unpackedBinaries(ctx, state)
	if err != nil {
		return nil, err
	}
	instance, err := claim(state, options.Stable)
	if err != nil {
		return nil, err
	}
	instance.binaries = binaries
	if err := instance.run(ctx); err != nil {
		return nil, errors.Join(err, instance.Stop())
	}
	return instance, nil
}

// URL is the connection URL of the server's postgres database, for the superuser postgres over the
// Unix socket; the server trusts every local connection. The port names the socket file, so
// PGPORT cannot point a client at another one.
func (i *Instance) URL() string {
	query := url.Values{"host": {i.socket}, "port": {strconv.Itoa(socketPort)}, "sslmode": {"disable"}}.Encode()
	address := url.URL{
		Scheme:   "postgres",
		User:     url.User(superuser),
		Path:     "/postgres",
		RawQuery: strings.ReplaceAll(query, "+", "%20"), // libpq and pgx read + as itself; Encode escapes a real + as %2B
	}
	return address.String()
}

// Directory is the server's directory.
func (i *Instance) Directory() string { return i.directory }

// Stop stops the server at once, removes its directory and releases it.
func (i *Instance) Stop() error {
	var stopError error
	if _, err := os.Stat(filepath.Join(i.directory, dataName, "postmaster.pid")); err == nil {
		stopError = i.pgControl(context.Background(), "stop", "-w", "-m", "immediate", "-D", filepath.Join(i.directory, dataName))
	}
	return errors.Join(stopError, i.release())
}

// release empties the directory while it still holds the lock, so no other process can claim it
// in between. A stable directory keeps its lock file: a claimer that opened it meanwhile gets the
// lock of the same file, and so the directory, once this process lets go.
func (i *Instance) release() error {
	var removeError error
	if i.stable {
		removeError = clearDirectory(i.directory)
	} else {
		removeError = os.RemoveAll(i.directory)
	}
	if i.socket != "" && i.socket != i.directory {
		removeError = errors.Join(removeError, os.RemoveAll(i.socket))
	}
	return errors.Join(removeError, i.lock.Close()) // closing the file releases the lock
}

func (i *Instance) run(ctx context.Context) error {
	socket, err := socketDirectory(i.directory)
	if err != nil {
		return err
	}
	i.socket = socket
	data := filepath.Join(i.directory, dataName)
	if err := i.command(ctx, "initdb", "-D", data, "-U", superuser, "-A", "trust", "-E", "UTF8", "--locale=C", "--no-sync", "--no-instructions"); err != nil {
		return err
	}
	if err := appendSettings(filepath.Join(data, "postgresql.conf"), i.socket); err != nil {
		return err
	}
	return i.pgControl(ctx, "start", "-w", "-t", startTimeout, "-D", data, "-l", filepath.Join(i.directory, logName))
}

func (i *Instance) pgControl(ctx context.Context, arguments ...string) error {
	return i.command(ctx, "pg_ctl", arguments...)
}

// command runs a PostgreSQL program with its output appended to the server's log, and returns
// the end of the log with its error.
func (i *Instance) command(ctx context.Context, program string, arguments ...string) error {
	logPath := filepath.Join(i.directory, logName)
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: the log of the server's own directory
	if err != nil {
		return fmt.Errorf("open the server log: %w", err)
	}
	run := exec.CommandContext(ctx, filepath.Join(i.binaries, "bin", program), arguments...) //nolint:gosec // G204: a PostgreSQL program of the unpacked binaries
	run.Stdout = log
	run.Stderr = log
	runError := run.Run()
	if closeError := log.Close(); runError == nil && closeError != nil {
		return fmt.Errorf("write the server log: %w", closeError)
	}
	if runError != nil {
		return fmt.Errorf("%s %s: %w\n%s", program, arguments[0], runError, logTail(logPath))
	}
	return nil
}

func appendSettings(configuration, socketDirectory string) error {
	file, err := os.OpenFile(configuration, os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // G304: postgresql.conf of the server's own data directory
	if err != nil {
		return fmt.Errorf("open postgresql.conf: %w", err)
	}
	lines := append([]string{"", "# postgrestest"}, settings...)
	// a list of directories: double quotes keep a comma or a space in the path
	lines = append(lines, "unix_socket_directories = "+quoteSetting(`"`+strings.ReplaceAll(socketDirectory, `"`, `""`)+`"`), "")
	_, writeError := file.WriteString(strings.Join(lines, "\n"))
	if err := errors.Join(writeError, file.Close()); err != nil {
		return fmt.Errorf("write postgresql.conf: %w", err)
	}
	return nil
}

// socketDirectory is directory when the socket path fits the platform's limit, else a directory of
// this user below /tmp named after a digest of directory, so a stable directory keeps its URL.
//
// shortcut: the sweep does not remove the short socket directory of a dead process's server; the
// next server of a stable directory reuses it, /tmp is cleared at boot.
func socketDirectory(directory string) (string, error) {
	if len(filepath.Join(directory, socketName)) <= maximumSocketPath() {
		return directory, nil
	}
	digest := sha256.Sum256([]byte(directory))
	short := filepath.Join(shortSocketParent, shortSocketPrefix+hex.EncodeToString(digest[:8]))
	if err := os.Mkdir(short, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("create the socket directory: %w", err)
	}
	// the server trusts whoever reaches the socket: a directory another user made would let them in
	if err := privateDirectory(short); err != nil {
		return "", fmt.Errorf("the socket directory %s: %w", short, err)
	}
	return short, nil
}

// maximumSocketPath is the longest Unix socket path PostgreSQL accepts: sun_path is 108 bytes on
// Linux and 104 on macOS and the BSDs, with the terminating NUL.
func maximumSocketPath() int {
	if runtime.GOOS == "linux" {
		return 107
	}
	return 103
}

// quoteSetting quotes a string value of postgresql.conf.
func quoteSetting(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func logTail(path string) string {
	content, err := os.ReadFile(path) //nolint:gosec // G304: the log of the server's own directory
	if err != nil {
		return "(no server log: " + err.Error() + ")"
	}
	if len(content) > logTailLength {
		content = content[len(content)-logTailLength:]
	}
	return string(content)
}

func cacheDirectory() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the home directory for the PostgreSQL binaries: %w", err)
	}
	return filepath.Join(home, ".embedded-postgres-go"), nil
}
