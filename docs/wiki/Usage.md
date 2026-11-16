# Usage

```go
package store_test

var postgres = postgrestest.New(&postgrestest.Options{
	Migrate:     postgrestest.Statements(store.MigrationV1),
	Fingerprint: store.MigrationV1,
})

func TestMain(m *testing.M) { goleak.VerifyTestMain(postgres.Main(m)) }

func TestCreate(t *testing.T) {
	t.Parallel()
	pool := postgres.Pool(t)
	...
}
```

- `New` starts nothing. The first `Database` or `Pool` call starts the server (or attaches to `POSTGRESTEST_URL`) and builds the template; parallel tests wait for it.
- `Database(t)` returns a `postgres://` URL of a new database, a copy of the template. It is dropped when the test ends, after the cleanups the test registers later, so a pool opened on it is closed first. Hand the URL to a process the test starts; it reaches the same database.
- `Pool(t)` is `pgxpool.New` on `Database(t)` with pgx's defaults, closed when the test ends. For other pool settings, parse the URL with `pgxpool.ParseConfig`.
- `Main(m)` closes the server after the tests, before goleak looks for leaked goroutines. Without goleak: `os.Exit(postgres.Main(m).Run())`.

## Templates and fingerprints

`Options.Migrate` runs once per server on a connection to the fresh template database. `Statements(texts...)` runs each text in a transaction of its own, like goose runs a migration file. A Migrate of your own can do anything with the connection; close every other connection it opens, or the template cannot be published.

`Options.Fingerprint` identifies what Migrate builds. It matters only on a server that outlives the test run (an external one `POSTGRESTEST_URL` names); an embedded server and the launcher's are new in every run, so every run builds the template, fingerprint or not (tens of milliseconds for the govengers libraries). On a long-lived server a template with the same fingerprint is reused, a new fingerprint builds a new one, and the package's templates nobody used for a day are dropped. Use the SQL itself for constants (`Fingerprint: MigrationV1 + MigrationV2`) or `FingerprintFS(migrations)` for an `embed.FS`. Empty means a template for this process only, dropped by `Close`.

## goose

```go
var postgres *postgrestest.Server

//go:embed migrations/*.sql
var migrations embed.FS

func migrate(ctx context.Context, connection *pgx.Conn) error {
	files, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	database := stdlib.OpenDB(*connection.Config())
	defer database.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, database, files)
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
}

func TestMain(m *testing.M) {
	fingerprint, err := postgrestest.FingerprintFS(migrations)
	if err != nil {
		panic(err)
	}
	postgres = postgrestest.New(&postgrestest.Options{Migrate: migrate, Fingerprint: fingerprint})
	goleak.VerifyTestMain(postgres.Main(m))
}
```

`database.Close()` must run before Migrate returns (the `defer` does): a connection left open fails the build.

## Several templates

Tests that need another schema (an upgrade from an older version, a table the rest must not see) get a second Server on the same PostgreSQL:

```go
var postgresV1 = postgres.WithTemplate(&postgrestest.Options{Migrate: postgrestest.Statements(store.MigrationV1), Fingerprint: store.MigrationV1})
```

`Main` or `Close` of either closes both. Extra statements for one test can also run on that test's pool.

## The embedded server

Unix socket only, trust authentication for local connections, superuser `postgres`. `fsync`, `synchronous_commit`, `full_page_writes`, `autovacuum` and `jit` off, `max_connections = 500`, `TimeZone` UTC, locale C, UTF-8. It keeps nothing after it stops. The binaries are unpacked once per machine into `~/.embedded-postgres-go/postgrestest`, next to embedded-postgres' archive cache.
