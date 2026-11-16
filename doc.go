// Package postgrestest gives Go tests a real PostgreSQL 18: one server per test binary, or one
// for a whole test run, and a database of its own for every test, copied from a template the
// package's migrations built once.
//
// A package keeps one Server in a package variable and closes it from TestMain:
//
//	var postgres = postgrestest.New(&postgrestest.Options{
//		Migrate:     postgrestest.Statements(store.MigrationV1),
//		Fingerprint: store.MigrationV1,
//	})
//
//	func TestMain(m *testing.M) { goleak.VerifyTestMain(postgres.Main(m)) }
//
//	func TestCreate(t *testing.T) {
//		t.Parallel()
//		pool := postgres.Pool(t) // a fresh database with the tables of MigrationV1
//		...
//	}
//
// # Where the server comes from
//
// The first Database or Pool call starts the server. With POSTGRESTEST_URL set (a postgres://
// URL of a superuser on the maintenance database), the tests attach to that server and use it as
// is. Otherwise the test binary starts an embedded PostgreSQL that only it uses and that Main
// stops. The command cmd/postgrestest starts one server for every test binary of a run and sets
// POSTGRESTEST_URL for them:
//
//	go run github.com/govengers/postgrestest/cmd/postgrestest go test -race ./...
//
// An embedded server listens on a Unix socket only, trusts local connections, keeps nothing on
// disk after it stops and runs with fsync, synchronous commit, full-page writes, autovacuum and
// JIT off, UTC as its time zone and 500 connections. The binaries are unpacked once per machine
// into ~/.embedded-postgres-go/postgrestest, next to the archive embedded-postgres downloads, with
// the system tar when it can. The servers of test processes that died (a test timeout, a kill)
// are stopped by the next start; servers of live processes, in this checkout or another, are never
// touched.
//
// # Databases
//
// Every test database is a copy of the template (CREATE DATABASE ... TEMPLATE), dropped when the
// test ends, so tests may call t.Parallel and leave whatever they like behind. The template is
// built by Options.Migrate once per server. On a server that outlives the test binary (one that
// POSTGRESTEST_URL names), a template built for the same Options.Fingerprint is reused, and
// templates of older fingerprints are dropped. Database settings made with ALTER DATABASE ... SET
// are not copied by PostgreSQL; set them on the connection or the pool instead.
//
// # The Go test cache
//
// go test replays a passing package's result until its test binary, an environment variable
// it read or a file inside the module it opened changes. This package reads POSTGRESTEST_URL,
// HOME and PATH, and pgx reads its PG* variables; it opens files only below
// ~/.embedded-postgres-go, outside every module, which go test does not hash. An embedded server's
// socket path is never an input. The launcher's URL is the same in every run of one checkout, so
// launcher runs hit the cache too; switching between a launcher run and a plain go test run
// changes POSTGRESTEST_URL and reruns the packages once.
package postgrestest
