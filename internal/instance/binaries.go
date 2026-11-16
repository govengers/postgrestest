package instance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

const (
	binariesPrefix = "postgres-"
	unpackingName  = ".unpack-"
	unpackLockName = "unpack.lock"
)

// programs are the binaries a server directory needs; the archive holds more.
var programs = []string{"initdb", "pg_ctl", "postgres"}

// unpackedBinaries unpacks the PostgreSQL binaries once per machine and returns their directory.
// One process at a time unpacks, into a scratch directory it renames into place, so a killed
// process never leaves a half-unpacked directory behind and parallel test binaries wait instead
// of unpacking the same archive several times.
func unpackedBinaries(ctx context.Context, state string) (string, error) {
	final := filepath.Join(state, binariesPrefix+string(Version))
	if complete(final) {
		return final, nil
	}
	lock, err := waitForLock(filepath.Join(state, unpackLockName))
	if err != nil {
		return "", err
	}
	defer func() { _ = lock.Close() }() // closing releases the lock; nothing was written to it
	if complete(final) {
		return final, nil // another process unpacked them while this one waited
	}
	removeUnpackLeftovers(state)
	scratch, err := os.MkdirTemp(state, unpackingName)
	if err != nil {
		return "", fmt.Errorf("create the unpack directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }() // only the renamed copy matters
	unpacked := filepath.Join(scratch, "binaries")
	if err := unpack(ctx, scratch, unpacked); err != nil {
		return "", fmt.Errorf("unpack PostgreSQL %s: %w", Version, err)
	}
	if err := os.RemoveAll(final); err != nil { // an incomplete copy someone damaged
		return "", fmt.Errorf("replace the PostgreSQL binaries: %w", err)
	}
	if err := os.Rename(unpacked, final); err != nil {
		return "", fmt.Errorf("move the PostgreSQL binaries into place: %w", err)
	}
	return final, nil
}

func complete(binaries string) bool {
	for _, program := range programs {
		if _, err := os.Stat(filepath.Join(binaries, "bin", program)); err != nil {
			return false
		}
	}
	return true
}

// removeUnpackLeftovers removes the scratch directories of processes killed while unpacking; the
// caller holds the unpack lock, so none of them is in use.
func removeUnpackLeftovers(state string) {
	leftovers, _ := filepath.Glob(filepath.Join(state, unpackingName+"*")) // the pattern is well-formed
	for _, leftover := range leftovers {
		_ = os.RemoveAll(leftover) // a leftover is swept again next time
	}
}

// unpack unpacks the archive in embedded-postgres' cache, downloading it first when it is missing,
// with the system tar: about 2 s, where the library's pure Go xz decoder takes 30 to 60 s inside a
// -race test binary. Without a usable tar, or when the download did not land where expected, the
// library unpacks.
func unpack(ctx context.Context, scratch, unpacked string) error {
	archive, err := archivePath()
	if err != nil {
		return unpackByStarting(scratch, unpacked)
	}
	if _, err := os.Stat(archive); err != nil {
		downloadArchive(scratch)
	}
	if untar(ctx, archive, unpacked) == nil {
		return nil
	}
	return unpackByStarting(scratch, unpacked)
}

func untar(ctx context.Context, archive, directory string) error {
	if _, err := os.Stat(archive); err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if output, err := exec.CommandContext(ctx, "tar", "-xJf", archive, "-C", directory).CombinedOutput(); err != nil { //nolint:gosec // G204: the archive path is built from embedded-postgres' naming
		return errors.Join(fmt.Errorf("tar: %w: %s", err, output), os.RemoveAll(directory))
	}
	if !complete(directory) {
		return errors.Join(errors.New("the archive lacks a PostgreSQL program"), os.RemoveAll(directory))
	}
	return nil
}

// archivePath names the archive in embedded-postgres' cache the way the library does on the
// platforms the govengers CI and developers use.
func archivePath() (string, error) {
	cache, err := cacheDirectory()
	if err != nil {
		return "", err
	}
	platform, err := archivePlatform(runtime.GOOS, runtime.GOARCH, alpine())
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "embedded-postgres-binaries-"+platform+"-"+string(Version)+".txz"), nil
}

func archivePlatform(operatingSystem, architecture string, alpine bool) (string, error) {
	switch {
	case operatingSystem == "darwin" && architecture == "arm64":
		return "darwin-arm64v8", nil
	case operatingSystem == "darwin" && architecture == "amd64":
		return "darwin-amd64", nil
	case operatingSystem == "linux" && (architecture == "amd64" || architecture == "arm64"):
		platform := "linux-" + strings.Replace(architecture, "arm64", "arm64v8", 1)
		if alpine {
			platform += "-alpine"
		}
		return platform, nil
	}
	return "", fmt.Errorf("no archive name for %s/%s", operatingSystem, architecture)
}

func alpine() bool {
	_, err := os.Stat("/etc/alpine-release")
	return err == nil
}

// downloadArchive lets embedded-postgres download the archive into its cache without unpacking
// it. The library downloads only as part of a start, and a start whose binaries directory sits in a
// directory it cannot write to fails right after the download, before the xz decoder runs. The
// caller checks for the archive afterwards, so a library version that orders these steps
// differently only costs the slow path.
func downloadArchive(scratch string) {
	defer closeDownloadConnections()
	blocked := filepath.Join(scratch, "download")
	if err := os.Mkdir(blocked, 0o500); err != nil {
		return
	}
	defer func() { _ = os.Chmod(blocked, 0o700) }() //nolint:gosec // G302: a directory, writable again so the caller can remove the scratch directory
	port, err := freePort()
	if err != nil {
		return
	}
	server := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(Version).
		Port(port).
		BinariesPath(filepath.Join(blocked, "binaries")).
		RuntimePath(filepath.Join(scratch, "download-runtime")).
		Logger(io.Discard))
	if server.Start() == nil { // only when the directory was writable after all (root)
		_ = server.Stop() // the binaries are unpacked again with tar
	}
}

// unpackByStarting lets embedded-postgres download the archive, when it is not cached, and unpack
// it into unpacked: the library does both only as part of starting a server.
func unpackByStarting(scratch, unpacked string) error {
	defer closeDownloadConnections()
	port, err := freePort()
	if err != nil {
		return err
	}
	server := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(Version).
		Port(port).
		BinariesPath(unpacked).
		RuntimePath(filepath.Join(scratch, "runtime")).
		Logger(io.Discard))
	if err := server.Start(); err != nil {
		return err
	}
	return server.Stop()
}

// closeDownloadConnections closes the keep-alive connection embedded-postgres leaves open after it
// downloaded the archive with http.Get: its read loop would outlive the tests and fail a goroutine
// leak check on a cold machine.
func closeDownloadConnections() {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}

func freePort() (uint32, error) {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = listener.Close() }()                 // only the port number was needed
	return uint32(listener.Addr().(*net.TCPAddr).Port), nil //nolint:gosec // G115: a TCP port fits in uint32
}
