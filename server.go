package postgrestest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
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

// cleanupTimeout bounds the drop of a test's database after the test, from the moment the
// statement may run.
const cleanupTimeout = 30 * time.Second

// concurrentStatements bounds the CREATE DATABASE and DROP DATABASE statements one test binary
// runs at a time. Each forces checkpoints; dozens at once from the parallel tests of several
// packages slow a server to a fraction of its rate one after another, and drops then time out.
const concurrentStatements = 2

// errClosed fails the tests that ask a Server for a database after Close.
var errClosed = errors.New("the server is closed")

// Options configure a Server. The zero value gives every test an empty database.
type Options struct {
	// Migrate builds the template every test database is copied from, on a connection to the
	// fresh template database. It runs once per server and fingerprint, before the first test
	// database. nil leaves the template empty. Statements returns a Migrate for SQL texts.
	Migrate func(ctx context.Context, connection *pgx.Conn) error
	// Fingerprint identifies what Migrate builds, for example the migration SQL itself or
	// FingerprintFS of the migration files. A template with the same fingerprint that is already on
	// the server, left by an earlier run against the same POSTGRESTEST_URL, is reused. Empty means
	// the template is built for this process only and dropped by Close.
	Fingerprint string
}

// Server hands out the test databases of one template on the PostgreSQL of one test binary. The
// PostgreSQL starts on the first Database call: the binary attaches to the server
// POSTGRESTEST_URL names, or else starts an embedded PostgreSQL 18 that only it uses. Main stops it
// after the tests. A Server is safe for concurrent use, so tests that call t.Parallel share it.
type Server struct {
	cluster     *cluster
	migrate     func(ctx context.Context, connection *pgx.Conn) error
	fingerprint string

	mutex         sync.Mutex
	scope         string // identifies the package on a shared server; part of every database name
	template      string // the built template, empty until the first Database
	templateError error  // a failed build, which every later Database returns
}

// cluster is the PostgreSQL the Servers of one test binary share.
type cluster struct {
	address    string        // overrides POSTGRESTEST_URL; set by this package's tests only
	statements chan struct{} // the slots of concurrentStatements

	mutex            sync.Mutex
	started          bool
	startError       error
	closed           bool
	embedded         *instance.Instance // nil when attached to POSTGRESTEST_URL
	base             *url.URL           // the maintenance database
	administration   *pgxpool.Pool
	processTemplates []string // templates without a fingerprint, dropped by close
}

// New returns a Server that starts when a test first asks for a database. It reads nothing and
// starts nothing before that. A nil options means the zero Options.
//
// One Server per test binary, kept in a package variable and closed by TestMain:
//
//	var postgres = postgrestest.New(&postgrestest.Options{
//		Migrate:     postgrestest.Statements(store.MigrationV1),
//		Fingerprint: store.MigrationV1,
//	})
//
//	func TestMain(m *testing.M) { goleak.VerifyTestMain(postgres.Main(m)) }
func New(options *Options) *Server {
	return newServer(newCluster(""), options)
}

func newCluster(address string) *cluster {
	return &cluster{address: address, statements: make(chan struct{}, concurrentStatements)}
}

// WithTemplate returns a Server on the same PostgreSQL whose test databases copy another template,
// for the tests that need other migrations than the rest (an upgrade from an older schema, for
// example). The two share one start and one stop: Main or Close of either closes both.
func (s *Server) WithTemplate(options *Options) *Server {
	return newServer(s.cluster, options)
}

func newServer(shared *cluster, options *Options) *Server {
	if options == nil {
		options = &Options{}
	}
	return &Server{cluster: shared, migrate: options.Migrate, fingerprint: options.Fingerprint}
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
		if err := s.cluster.dropDatabase(name); err != nil {
			tb.Errorf("postgrestest: %v", err)
		}
	})
	return s.cluster.databaseURL(name)
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

// Close drops the templates built for this process only, closes the server's connections and
// stops the embedded server, for this Server and every one that shares its PostgreSQL through
// WithTemplate. Close of a Server that never started does nothing. After Close, Database fails the
// test.
func (s *Server) Close() error {
	return s.cluster.close()
}

func (c *cluster) close() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if !c.started || c.startError != nil {
		return nil
	}
	var errs []error
	if c.embedded == nil { // an embedded server takes its templates with it
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		for _, template := range c.processTemplates {
			errs = append(errs, dropTemplate(ctx, c.administration, template))
		}
	}
	c.administration.Close()
	if c.embedded != nil {
		errs = append(errs, c.embedded.Stop())
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("postgrestest: close: %w", err)
	}
	return nil
}

// start attaches to POSTGRESTEST_URL or starts the embedded server, once. The start is not bound
// to the first test's context: every later test needs it too.
func (c *cluster) start(ctx context.Context) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.closed {
		return errClosed
	}
	if c.started {
		return c.startError
	}
	c.started = true
	c.startError = c.connect(context.WithoutCancel(ctx))
	return c.startError
}

func (c *cluster) connect(ctx context.Context) error {
	address := c.address
	if address == "" {
		address = os.Getenv(EnvironmentVariable)
	}
	if address == "" {
		embedded, err := instance.Start(ctx, &instance.Options{})
		if err != nil {
			return fmt.Errorf("start the embedded PostgreSQL: %w", err)
		}
		c.embedded = embedded
		address = embedded.URL()
	}
	base, err := parseURL(address)
	if err != nil {
		return errors.Join(err, c.stopEmbedded())
	}
	administration, err := pgxpool.New(ctx, address)
	if err == nil {
		err = administration.Ping(ctx)
	}
	if err != nil {
		if administration != nil {
			administration.Close()
		}
		return errors.Join(fmt.Errorf("connect to %s: %w", base.Redacted(), err), c.stopEmbedded())
	}
	c.base = base
	c.administration = administration
	return nil
}

func (c *cluster) stopEmbedded() error {
	if c.embedded == nil {
		return nil
	}
	return c.embedded.Stop()
}

// inSlot runs statement once one of the concurrentStatements slots is free, waiting at most until
// ctx ends.
func (c *cluster) inSlot(ctx context.Context, statement func() error) error {
	select {
	case c.statements <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("wait for the server: %w", context.Cause(ctx))
	}
	defer func() { <-c.statements }()
	return statement()
}

// dropDatabase drops a test's database; its timeout starts when the statement may run, not
// while it waits for a slot behind the drops of other tests.
func (c *cluster) dropDatabase(name string) error {
	return c.inSlot(context.Background(), func() error {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		return dropDatabase(ctx, c.administration, name)
	})
}

// addProcessTemplate records a template close drops.
func (c *cluster) addProcessTemplate(name string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.processTemplates = append(c.processTemplates, name)
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

// databaseURL is the base URL on the named database. Query parameters that name a database
// would win over the path, so they go; the others stay as they were written.
func (c *cluster) databaseURL(name string) string {
	address := *c.base
	address.Path = "/" + name
	address.RawPath = ""
	var kept []string
	for pair := range strings.SplitSeq(address.RawQuery, "&") {
		key, _, _ := strings.Cut(pair, "=")
		if key, err := url.QueryUnescape(key); err == nil && (key == "dbname" || key == "database") {
			continue
		}
		kept = append(kept, pair)
	}
	address.RawQuery = strings.Join(kept, "&")
	return address.String()
}
