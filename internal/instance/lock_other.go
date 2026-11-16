//go:build !unix

package instance

import (
	"errors"
	"os"
)

var errUnsupported = errors.New("postgrestest runs embedded servers on Unix-like systems only; set POSTGRESTEST_URL to a running server")

func lockFile(string, bool) (*os.File, bool, error) { return nil, false, errUnsupported }

func waitForLock(string) (*os.File, error) { return nil, errUnsupported }
