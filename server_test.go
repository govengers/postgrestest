package postgrestest

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

const schema = `CREATE TABLE tasks (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, title text NOT NULL)`

// primary is the one server of this test binary (TST-002); the tests of attached servers point a
// cluster of their own at its URL.
var primary = New(&Options{Migrate: Statements(schema), Fingerprint: schema})

func TestMain(m *testing.M) { goleak.VerifyTestMain(primary.Main(m)) }

func TestDatabaseIsACopyOfTheTemplate(t *testing.T) {
	t.Parallel()
	pool := primary.Pool(t)

	_, err := pool.Exec(t.Context(), "INSERT INTO tasks (title) VALUES ('write the tests')")
	require.NoError(t, err)
	require.Equal(t, 1, count(t, pool, "SELECT count(*) FROM tasks"))
}

func TestDatabasesAreIndependent(t *testing.T) {
	t.Parallel()
	first, second := primary.Pool(t), primary.Pool(t)

	_, err := first.Exec(t.Context(), "INSERT INTO tasks (title) VALUES ('only here')")
	require.NoError(t, err)
	require.Equal(t, 0, count(t, second, "SELECT count(*) FROM tasks"))
}

func TestDatabaseIsDroppedAfterTheTest(t *testing.T) {
	t.Parallel()
	var name string
	t.Run("test", func(t *testing.T) {
		require.NoError(t, primary.Pool(t).QueryRow(t.Context(), "SELECT current_database()").Scan(&name))
	})

	require.Equal(t, 0, count(t, primary.cluster.administration, "SELECT count(*) FROM pg_database WHERE datname = $1", name))
}

func TestParallelTestsGetDatabasesOfTheirOwn(t *testing.T) {
	t.Parallel()
	for index := range 16 {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			t.Parallel()
			pool := primary.Pool(t)
			_, err := pool.Exec(t.Context(), "INSERT INTO tasks (title) VALUES ($1)", fmt.Sprint(index))
			require.NoError(t, err)
			require.Equal(t, 1, count(t, pool, "SELECT count(*) FROM tasks"))
		})
	}
}

func TestEmbeddedServerIsTunedForTests(t *testing.T) {
	t.Parallel()
	pool := primary.Pool(t)
	if primary.cluster.embedded == nil {
		t.Skip("attached to " + EnvironmentVariable + ", whose settings are its own")
	}
	for setting, expected := range map[string]string{"fsync": "off", "synchronous_commit": "off", "TimeZone": "UTC", "listen_addresses": "", "max_connections": "500"} {
		var actual string
		require.NoError(t, pool.QueryRow(t.Context(), "SELECT current_setting($1)", setting).Scan(&actual))
		require.Equal(t, expected, actual, setting)
	}
}

func TestTemplateRejectsConnections(t *testing.T) {
	t.Parallel()
	primary.Database(t) // the template exists once a database was copied from it
	address := primary.cluster.databaseURL(primary.template)

	connection, err := pgx.Connect(t.Context(), address)
	if err == nil {
		_ = connection.Close(t.Context()) // the assertion below reports the unexpected connection
	}
	require.ErrorContains(t, err, "not currently accepting connections")
}

// attached returns a server of the given scope that attaches to the primary like a test binary of
// a launcher run; it is closed when the test ends.
func attached(t *testing.T, scope string, options *Options) *Server {
	t.Helper()
	primary.Database(t) // starts the primary
	server := newServer(newCluster(primary.cluster.base.String()), options)
	server.scope = scope
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	return server
}

// unique makes a scope of its own for every run of a test, so -count=2 and a server that outlives
// the binary do not find the templates of an earlier run.
func unique(scope string) string { return scope + "_" + rand.Text()[:6] }

// countingMigrate is a Migrate that applies the schema and counts its calls.
func countingMigrate(calls *atomic.Int32) func(ctx context.Context, connection *pgx.Conn) error {
	return func(ctx context.Context, connection *pgx.Conn) error {
		calls.Add(1)
		return Statements(schema)(ctx, connection)
	}
}

func TestAttachedServerReusesTheTemplateOfItsFingerprint(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	options := &Options{Migrate: countingMigrate(&calls), Fingerprint: "reuse"}
	scope := unique("reuse")
	earlier := attached(t, scope, options)
	createAndDrop(t, earlier)
	require.NoError(t, earlier.Close())

	later := attached(t, scope, options)
	pool, err := pgxpool.New(t.Context(), later.Database(t))
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.Equal(t, int32(1), calls.Load(), "the later server copies the earlier template")
	require.Equal(t, 0, count(t, pool, "SELECT count(*) FROM tasks"))
}

func TestTemplatesOfThePackageUnusedForADayAreDropped(t *testing.T) {
	t.Parallel()
	unused := attached(t, "unused", &Options{Migrate: Statements(schema), Fingerprint: "unused"})
	createAndDrop(t, unused)
	lastUsed := time.Now().Add(-unusedTemplateAge - time.Minute).UTC().Format(time.RFC3339)
	_, err := primary.cluster.administration.Exec(t.Context(), "COMMENT ON DATABASE "+identifier(unused.template)+" IS "+literal(lastUsed))
	require.NoError(t, err)
	recent := attached(t, "unused", &Options{Migrate: Statements(schema), Fingerprint: "recent"})
	createAndDrop(t, recent)

	current := attached(t, "unused", &Options{Migrate: Statements(schema), Fingerprint: "current"})
	createAndDrop(t, current)

	require.False(t, onServer(t, unused.template))
	require.True(t, onServer(t, recent.template), "a run with other migrations may still copy it")
	require.True(t, onServer(t, current.template))
}

func TestWithTemplateSharesThePostgreSQL(t *testing.T) {
	t.Parallel()
	upgrade := primary.WithTemplate(&Options{Migrate: Statements("CREATE TABLE old_tasks (id int)"), Fingerprint: "old"})

	current, older := primary.Pool(t), upgrade.Pool(t)

	require.Equal(t, 1, count(t, current, "SELECT count(*) FROM pg_tables WHERE tablename = 'tasks'"))
	require.Equal(t, 1, count(t, older, "SELECT count(*) FROM pg_tables WHERE tablename = 'old_tasks'"))
	require.Equal(t, 0, count(t, older, "SELECT count(*) FROM pg_tables WHERE tablename = 'tasks'"))
	require.Equal(t, setting(t, current, "data_directory"), setting(t, older, "data_directory"))
}

func TestCloseOfATemplateClosesTheSharedPostgreSQL(t *testing.T) {
	t.Parallel()
	root := attached(t, "shared", nil)
	other := root.WithTemplate(&Options{Migrate: Statements("CREATE TABLE notes (id int)")})
	createAndDrop(t, other)
	template := other.template

	require.NoError(t, other.Close())

	_, err := root.createDatabase(t.Context())
	require.ErrorIs(t, err, errClosed)
	require.False(t, onServer(t, template), "the template without a fingerprint went with the close")
}

func setting(t *testing.T, database querier, name string) string {
	t.Helper()
	var value string
	require.NoError(t, database.QueryRow(t.Context(), "SELECT current_setting($1)", name).Scan(&value))
	return value
}

func onServer(t *testing.T, name string) bool {
	t.Helper()
	return count(t, primary.cluster.administration, "SELECT count(*) FROM pg_database WHERE datname = $1", name) == 1
}

func TestTemplateWithoutFingerprintLastsOneProcess(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	first := attached(t, "process", &Options{Migrate: countingMigrate(&calls)})
	createAndDrop(t, first)
	template := first.template
	second := attached(t, "process", &Options{Migrate: countingMigrate(&calls)})
	second.Database(t)

	require.Equal(t, int32(2), calls.Load(), "every server builds its own")
	require.NoError(t, first.Close())
	require.False(t, onServer(t, template))
}

func TestDroppedTemplateFailsTheDatabase(t *testing.T) {
	t.Parallel()
	server := attached(t, "dropped", &Options{Migrate: Statements(schema), Fingerprint: "dropped"})
	createAndDrop(t, server)
	require.NoError(t, dropTemplate(t.Context(), primary.cluster.administration, server.template)) // by hand, or by a run a day later

	_, err := server.createDatabase(t.Context())

	require.ErrorContains(t, err, "copy the template")
	require.ErrorContains(t, err, "does not exist")
}

func TestServersOfOnePackageBuildTheTemplateOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	options := &Options{Migrate: countingMigrate(&calls), Fingerprint: "concurrent"}
	scope := unique("concurrent")
	servers := []*Server{attached(t, scope, options), attached(t, scope, options), attached(t, scope, options)}
	errs := make([]error, len(servers))
	var group sync.WaitGroup
	for index, server := range servers {
		group.Go(func() {
			var name string
			name, errs[index] = server.createDatabase(t.Context())
			if errs[index] == nil {
				errs[index] = dropDatabase(context.Background(), server.cluster.administration, name)
			}
		})
	}
	group.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int32(1), calls.Load())
}

func TestFailedMigrateFailsTheDatabaseAndLeavesNoTemplate(t *testing.T) {
	t.Parallel()
	server := attached(t, "failing", &Options{Migrate: Statements(schema, "CREATE TABLE broken ("), Fingerprint: "failing"})

	_, err := server.createDatabase(t.Context())

	require.ErrorContains(t, err, "migrate the template: statements 2")
	require.Equal(t, 0, count(t, primary.cluster.administration, "SELECT count(*) FROM pg_database WHERE starts_with(datname, $1)", server.name("")))
}

func TestFailedTemplateBuildFailsEveryLaterDatabaseAtOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := attached(t, unique("remembered"), &Options{Migrate: func(context.Context, *pgx.Conn) error {
		calls.Add(1)
		return errors.New("broken")
	}})
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	_, endedError := server.createDatabase(ended)

	_, first := server.createDatabase(t.Context())
	_, second := server.createDatabase(t.Context())

	require.Error(t, endedError)
	require.ErrorContains(t, first, "broken")
	require.Equal(t, first, second)
	require.Equal(t, int32(1), calls.Load(), "a build that failed because its test ended is no verdict")
}

// A builder that died after marking its scratch database leaves a template under the scratch name.
func TestTemplateBuildDropsAMarkedScratchDatabase(t *testing.T) {
	t.Parallel()
	server := attached(t, unique("scratch"), &Options{Migrate: Statements(schema), Fingerprint: "scratch"})
	scratch := identifier(server.name(buildSuffix))
	for _, statement := range []string{"CREATE DATABASE " + scratch, "ALTER DATABASE " + scratch + " WITH IS_TEMPLATE true ALLOW_CONNECTIONS false"} {
		_, err := primary.cluster.administration.Exec(t.Context(), statement)
		require.NoError(t, err)
	}

	createAndDrop(t, server)

	require.False(t, onServer(t, server.name(buildSuffix)))
	require.True(t, onServer(t, server.template))
}

func TestCopiesWaitForAFreeSlot(t *testing.T) {
	t.Parallel()
	server := attached(t, unique("slots"), nil)
	createAndDrop(t, server) // starts the cluster and builds the template
	for range concurrentStatements {
		server.cluster.statements <- struct{}{}
	}
	ended, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := server.createDatabase(ended)
	for range concurrentStatements {
		<-server.cluster.statements
	}

	require.ErrorContains(t, err, "wait for the server")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 0, count(t, primary.cluster.administration, "SELECT count(*) FROM pg_database WHERE starts_with(datname, $1)", server.name(databaseKind)))
}

func TestDatabaseURLNamesOnlyTheDatabase(t *testing.T) {
	t.Parallel()
	base, err := parseURL("postgres://postgres@/postgres?dbname=postgres&host=/a+b%20c&database=postgres&sslmode=disable")
	require.NoError(t, err)

	address := (&cluster{base: base}).databaseURL("postgrestest_x")

	require.Equal(t, "postgres://postgres@/postgrestest_x?host=/a+b%20c&sslmode=disable", address)
	config, err := pgx.ParseConfig(address)
	require.NoError(t, err)
	require.Equal(t, "postgrestest_x", config.Database)
	require.Equal(t, "/a+b c", config.Host)
}

// New reads nothing (LIB-004): the scope comes from the working directory of the first Database.
func TestNewReadsNothingBeforeTheFirstDatabase(t *testing.T) {
	primary.Database(t) // starts the primary
	server := newServer(newCluster(primary.cluster.base.String()), &Options{Migrate: Statements(schema)})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	t.Chdir(t.TempDir())

	createAndDrop(t, server)

	require.Equal(t, packageScope(), server.scope)
}

func TestMigrateMustCloseWhatItOpens(t *testing.T) {
	t.Parallel()
	leaked := make(chan *pgx.Conn, 1)
	server := attached(t, "leaking", &Options{Fingerprint: "leaking", Migrate: func(ctx context.Context, connection *pgx.Conn) error {
		second, err := pgx.ConnectConfig(ctx, connection.Config())
		leaked <- second
		return err
	}})
	t.Cleanup(func() { _ = (<-leaked).Close(context.Background()) }) // the server already ended its session

	_, err := server.createDatabase(t.Context())

	require.ErrorContains(t, err, "close every connection Migrate opens")
	require.Equal(t, 0, count(t, primary.cluster.administration, "SELECT count(*) FROM pg_database WHERE starts_with(datname, $1)", server.name("")))
}

func TestEnvironmentVariableMustBeAPostgresURL(t *testing.T) {
	for _, address := range []string{"host=/tmp dbname=postgres", "mysql://localhost/test", "postgres://%zz"} {
		t.Run(address, func(t *testing.T) {
			t.Setenv(EnvironmentVariable, address)
			_, err := New(nil).createDatabase(t.Context())
			require.ErrorContains(t, err, "POSTGRESTEST_URL must be a postgres:// URL")
		})
	}
}

func TestUnreachableServerFailsEveryDatabase(t *testing.T) {
	t.Parallel()
	server := newServer(newCluster("postgres://postgres@/postgres?host=/nonexistent-postgrestest&connect_timeout=1"), nil)

	_, first := server.createDatabase(t.Context())
	_, second := server.createDatabase(t.Context())

	require.ErrorContains(t, first, "connect to postgres://postgres@/postgres")
	require.Equal(t, first, second, "the start is tried once")
	require.NoError(t, server.Close())
}

func TestClosedServerFails(t *testing.T) {
	server := New(nil)
	require.NoError(t, server.Close(), "a server that never started has nothing to close")

	_, err := server.createDatabase(t.Context())

	require.ErrorIs(t, err, errClosed)
	require.NoError(t, server.Close())
}

func TestMainClosesTheServerAfterTheTests(t *testing.T) {
	t.Parallel()
	server := attached(t, "main", nil)
	createAndDrop(t, server)

	code := server.Main(exitCode(3)).Run()

	require.Equal(t, 3, code)
	_, err := server.createDatabase(t.Context())
	require.ErrorIs(t, err, errClosed)
}

// createAndDrop copies the template once, for tests that close the server before they end.
func createAndDrop(t *testing.T, server *Server) {
	t.Helper()
	name, err := server.createDatabase(t.Context())
	require.NoError(t, err)
	require.NoError(t, dropDatabase(t.Context(), server.cluster.administration, name))
}

type exitCode int

func (e exitCode) Run() int { return int(e) }

func TestStatementsRunInTransactionsOfTheirOwn(t *testing.T) {
	t.Parallel()
	connection, err := pgx.Connect(t.Context(), primary.Database(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connection.Close(context.Background())) })

	err = Statements("CREATE TABLE first (id int)", "CREATE TABLE second (id int); SELECT 1/0")(t.Context(), connection)

	require.ErrorContains(t, err, "statements 2")
	require.Equal(t, 1, count(t, connection, "SELECT count(*) FROM pg_tables WHERE tablename IN ('first', 'second')"))
}

type querier interface {
	QueryRow(ctx context.Context, sql string, arguments ...any) pgx.Row
}

func count(t *testing.T, database querier, query string, arguments ...any) int {
	t.Helper()
	var result int
	require.NoError(t, database.QueryRow(t.Context(), query, arguments...).Scan(&result))
	return result
}

// BenchmarkDatabase measures what a test pays for its database: the copy and the drop.
func BenchmarkDatabase(b *testing.B) {
	primary.Database(b) // the start and the template are not part of it
	for b.Loop() {
		name, err := primary.createDatabase(b.Context())
		require.NoError(b, err)
		require.NoError(b, dropDatabase(b.Context(), primary.cluster.administration, name))
	}
}
