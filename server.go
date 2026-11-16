package postgrestest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/govengers/postgrestest/internal/instance"
)

// EnvironmentVariable names the variable that points the tests at a running server instead of an
// embedded one: a postgres:// URL of a superuser on its maintenance database, used as is. The
// launcher command cmd/postgrestest sets it for every test binary of one run.
const EnvironmentVariable = "POSTGRESTEST_URL"

// cleanupTimeout bounds the drop of a test's database after the test.
const cleanupTimeout = 30 * time.Second

// ErrClosed is returned, through the test's failure, by a Server used after Close.
var ErrClosed = errors.New("postgrestest: server closed")

// Options configure a Server. The zero value gives every test an empty database.
type Options struct {
	// Migrate builds the template every test database is cloned from, on a connection to the
	// fresh template database. It runs once per server and fingerprint, before the first test
	// database. nil leaves the template empty. Statements returns a Migrate for SQL texts.
	Migrate func(ctx context.Context, connection *pgx.Conn) error
	// Fingerprint identifies what Migrate builds, for example the migration SQL itself or
	// FingerprintFS of the migration files. A template with the same fingerprint that is already on
	// the server, left by an earlier run against the same POSTGRESTEST_URL, is reused; a new
	// fingerprint builds a new template and drops the older ones of the package. Empty means the
	// template is built for this process only and dropped by Close.
	Fingerprint string
}

// Server is the PostgreSQL of one test binary. It starts on the first Database call: it attaches
// to the server POSTGRESTEST_URL names, or else starts an embedded PostgreSQL 18 that only this
// process uses. Main stops it after the tests. A Server is safe for concurrent use, so tests that
// call t.Parallel share it.
type Server struct {
	migrate     func(ctx context.Context, connection *pgx.Conn) error
	fingerprint string
	scope       string // identifies the package on a shared server; part of every database name
	address     string // overrides POSTGRESTEST_URL; set by this package's tests only

	mutex          sync.Mutex
	started        bool
	startError     error
	closed         bool
	embedded       *instance.Instance // nil when attached to POSTGRESTEST_URL
	base           *url.URL           // the maintenance database
	administration *pgxpool.Pool
	template       string // the built template, empty until the first Database
}

// New returns a Server that starts when a test first asks for a database. It reads nothing and
// starts nothing before that. A nil options means the zero Options.
//
// One Server per test binary, kept in a package variable and passed to TestMain:
//
//	var postgres = postgrestest.New(&postgrestest.Options{
//		Migrate:     postgrestest.Statements(store.MigrationV1),
//		Fingerprint: store.MigrationV1,
//	})
//
//	func TestMain(m *testing.M) { goleak.VerifyTestMain(postgres.Main(m)) }
func New(options *Options) *Server {
	if options == nil {
		options = &Options{}
	}
	return &Server{migrate: options.Migrate, fingerprint: options.Fingerprint, scope: packageScope()}
}

// packageScope is a digest of the working directory, which go test sets to the package's
// directory: test binaries of other packages and other checkouts on one shared server keep apart.
func packageScope() string {
	directory, _ := os.Getwd() // an unknown directory still gives one scope for the process
	digest := sha256.Sum256([]byte(directory))
	return hex.EncodeToString(digest[:6])
}

// TestingM is the method of *testing.M that runs the tests; goleak.VerifyTestMain takes it too.
type TestingM interface {
	Run() int
}

// Main returns m with a Run that closes the server after the tests, so a goroutine leak check that
// runs afterwards sees none of the server's connections:
//
//	func TestMain(m *testing.M) { goleak.VerifyTestMain(postgres.Main(m)) }
//	func TestMain(m *testing.M) { os.Exit(postgres.Main(m).Run()) } // without goleak
//
// A failed Close fails the run with exit code 1 and its error on standard error, where go test
// reports a test binary's output.
func (s *Server) Main(m TestingM) TestingM {
	return &closingMain{tests: m, server: s}
}

type closingMain struct {
	tests  TestingM
	server *Server
}

func (c *closingMain) Run() int {
	code := c.tests.Run()
	if err := c.server.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err) // the test binary's own output, which go test reports
		return 1
	}
	return code
}

// Database returns the connection URL of a new database for this test, a copy of the template,
// and drops it when the test ends, after the cleanups the test registered later (a pool it opened
// on the URL, for example). A failure to start the server, build the template or copy it fails the
// test.
func (s *Server) Database(tb testing.TB) string {
	tb.Helper()
	name, err := s.createDatabase(tb.Context())
	if err != nil {
		tb.Fatalf("postgrestest: %v", err)
	}
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if err := dropDatabase(ctx, s.administration, name); err != nil {
			tb.Errorf("postgrestest: %v", err)
		}
	})
	return s.databaseURL(name)
}

// Pool returns a pgx pool on a new database for this test (see Database) with pgx's default
// settings, closed when the test ends.
func (s *Server) Pool(tb testing.TB) *pgxpool.Pool {
	tb.Helper()
	address := s.Database(tb)
	pool, err := pgxpool.New(tb.Context(), address)
	if err != nil {
		tb.Fatalf("postgrestest: open a pool: %v", err)
	}
	tb.Cleanup(pool.Close)
	return pool
}

// Close drops the template when it was built for this process only, closes the server's
// connections and stops the embedded server. Close of a Server that never started does nothing.
// After Close, Database fails the test with ErrClosed.
func (s *Server) Close() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if !s.started || s.startError != nil {
		return nil
	}
	var errs []error
	if s.fingerprint == "" && s.embedded == nil && s.template != "" {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		errs = append(errs, dropTemplate(ctx, s.administration, s.template))
	}
	s.administration.Close()
	if s.embedded != nil {
		errs = append(errs, s.embedded.Stop())
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("postgrestest: close: %w", err)
	}
	return nil
}

// start attaches to POSTGRESTEST_URL or starts the embedded server, once; the caller holds the
// mutex. The start is not bound to the first test's context: every later test needs it too.
func (s *Server) start(ctx context.Context) error {
	if s.closed {
		return ErrClosed
	}
	if s.started {
		return s.startError
	}
	s.started = true
	s.startError = s.connect(context.WithoutCancel(ctx))
	return s.startError
}

func (s *Server) connect(ctx context.Context) error {
	address := s.address
	if address == "" {
		address = os.Getenv(EnvironmentVariable)
	}
	if address == "" {
		embedded, err := instance.Start(ctx, &instance.Options{})
		if err != nil {
			return fmt.Errorf("start the embedded PostgreSQL: %w", err)
		}
		s.embedded = embedded
		address = embedded.URL()
	}
	base, err := parseURL(address)
	if err != nil {
		return errors.Join(err, s.stopEmbedded())
	}
	administration, err := pgxpool.New(ctx, address)
	if err == nil {
		err = administration.Ping(ctx)
	}
	if err != nil {
		if administration != nil {
			administration.Close()
		}
		return errors.Join(fmt.Errorf("connect to %s: %w", base.Redacted(), err), s.stopEmbedded())
	}
	s.base = base
	s.administration = administration
	return nil
}

func (s *Server) stopEmbedded() error {
	if s.embedded == nil {
		return nil
	}
	return s.embedded.Stop()
}

// parseURL accepts postgres:// and postgresql:// URLs; keyword/value strings cannot name another
// database reliably.
func parseURL(address string) (*url.URL, error) {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return nil, fmt.Errorf("%s must be a postgres:// URL", EnvironmentVariable)
	}
	return parsed, nil
}

func (s *Server) databaseURL(name string) string {
	address := *s.base
	address.Path = "/" + name
	address.RawPath = ""
	return address.String()
}
