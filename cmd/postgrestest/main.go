// Command postgrestest starts one PostgreSQL for a whole test run. It runs the command it is given
// with POSTGRESTEST_URL pointing at the server, so every test binary of the run attaches to it
// instead of starting its own, stops the server when the command ends and exits with the command's
// status:
//
//	go run github.com/govengers/postgrestest/cmd/postgrestest go test -race ./...
//
// The server listens on a Unix socket in a directory named after the working directory, so the
// URL, which go test counts among the inputs of every cached result, is the same in every run of
// one checkout. A second run in the same checkout while the first still runs gets a directory of
// its own, and its results miss the cache once. With POSTGRESTEST_URL already set, the command
// runs against that server and nothing is started.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/govengers/postgrestest"
	"github.com/govengers/postgrestest/internal/instance"
)

const usage = "usage: postgrestest command [arguments...]"

// Exit codes of the launcher itself; otherwise it exits with the command's.
const (
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) int {
	if len(arguments) > 0 && arguments[0] == "--" {
		arguments = arguments[1:]
	}
	if len(arguments) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return exitUsage
	}
	if os.Getenv(postgrestest.EnvironmentVariable) != "" {
		return execute(arguments, nil)
	}
	server, err := instance.Start(context.Background(), &instance.Options{Stable: checkoutName()})
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgrestest:", err)
		return exitFailure
	}
	code := execute(arguments, []string{postgrestest.EnvironmentVariable + "=" + server.URL()})
	if err := server.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "postgrestest: stop the server:", err)
		return max(code, exitFailure)
	}
	return code
}

// checkoutName names the server directory after the working directory: stable from run to run,
// different for every checkout.
func checkoutName() string {
	directory, _ := os.Getwd() // an unknown directory still gives a stable name
	digest := sha256.Sum256([]byte(directory))
	return hex.EncodeToString(digest[:8])
}

// execute runs the command with the extra environment and passes interrupts on to it: the
// launcher has to outlive the command to stop the server.
func execute(arguments, environment []string) int {
	command := exec.CommandContext(context.Background(), arguments[0], arguments[1:]...) //nolint:gosec // G204: running the given command is the launcher's purpose
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	command.Env = append(os.Environ(), environment...)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := command.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "postgrestest:", err)
		return exitFailure
	}
	done := make(chan struct{})
	forwarded := make(chan struct{})
	go forward(signals, command.Process, done, forwarded)
	err := command.Wait()
	close(done)
	<-forwarded
	var exitError *exec.ExitError
	switch {
	case errors.As(err, &exitError) && exitError.ExitCode() >= 0:
		return exitError.ExitCode()
	case err != nil:
		fmt.Fprintln(os.Stderr, "postgrestest:", err)
		return exitFailure
	}
	return 0
}

func forward(signals <-chan os.Signal, process *os.Process, done <-chan struct{}, forwarded chan<- struct{}) {
	defer close(forwarded)
	defer func() { _ = recover() }() // a failed forward must not take the server down with it (CON-001)
	for {
		select {
		case received := <-signals:
			_ = process.Signal(received) // the command may have exited already
		case <-done:
			return
		}
	}
}
