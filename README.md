# postgrestest

[![CI](https://github.com/govengers/postgrestest/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/govengers/postgrestest/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/govengers/postgrestest.svg)](https://pkg.go.dev/github.com/govengers/postgrestest)
[![License](https://img.shields.io/github/license/govengers/postgrestest)](LICENSE)

Fast PostgreSQL for Go tests: a real PostgreSQL 18 per test binary or per test run, and a database of its own for every test, copied from a template your migrations built once. No Docker. Module `github.com/govengers/postgrestest`, Go 1.26, Apache 2.0. Depends on pgx v5 and embedded-postgres only.

```sh
go get github.com/govengers/postgrestest
```

## Usage

```go
var postgres = postgrestest.New(&postgrestest.Options{
	Migrate:     postgrestest.Statements(store.MigrationV1),
	Fingerprint: store.MigrationV1,
})

func TestMain(m *testing.M) { goleak.VerifyTestMain(postgres.Main(m)) }

func TestCreate(t *testing.T) {
	t.Parallel()
	pool := postgres.Pool(t) // a fresh database with the tables of MigrationV1, dropped after the test
	...
}
```

- `Pool(t)` returns a pgx pool on a new database; `Database(t)` returns its URL, for a pool of your own or a process the test starts. Both are safe with `t.Parallel`.
- `Options.Migrate` builds the template once per server: `Statements(texts...)` for SQL constants, or a function of your own (goose, for example) on the `*pgx.Conn` it gets. `Options.Fingerprint` lets a long-lived server (an external `POSTGRESTEST_URL`) reuse the template; embedded and launcher servers are new in every run and build it every run whatever the fingerprint. `FingerprintFS` digests an `embed.FS` of migrations.
- `WithTemplate(options)` gives some tests other migrations on the same server.
- `Main(m)` closes the server after the tests, before goleak looks for leaks.

## Where the server comes from

- **Per test binary (default):** the first test that asks starts an embedded PostgreSQL 18 on a Unix socket, tuned for tests (fsync, synchronous commit, full-page writes, autovacuum and JIT off, UTC, 500 connections). Start about 0.5 s.
- **Per test run:** `go run github.com/govengers/postgrestest/cmd/postgrestest go test ./...` starts one server, sets `POSTGRESTEST_URL` for every test binary, stops it afterwards and exits with the command's status.
- **External:** `POSTGRESTEST_URL=postgres://...` (a superuser on the maintenance database) is used as is.

The binaries are unpacked once per machine into `~/.embedded-postgres-go/postgrestest` with the system tar (5 s instead of about 27 s for the pure-Go xz decoder under `-race`), next to embedded-postgres' archive cache, which the govengers CI restores. Servers of test processes that died are stopped by the next start; servers of live processes, in this checkout or another, are never touched.

## The Go test cache

Every test input stays the same between runs, so `go test` keeps replaying the results of unchanged packages: the package reads `POSTGRESTEST_URL`, `HOME`, `PATH` and pgx's `PG*` variables, and touches files only outside the module. The launcher's URL is the same in every run of a checkout. Details: [docs/wiki/Test-Cache.md](docs/wiki/Test-Cache.md).

## Docs

- [docs/wiki/Home.md](docs/wiki/Home.md): which piece for which need.
- [docs/wiki/Usage.md](docs/wiki/Usage.md), [Launcher](docs/wiki/Launcher.md), [Test cache](docs/wiki/Test-Cache.md), [Gotchas](docs/wiki/Gotchas.md).
- [docs/wiki/Migrating.md](docs/wiki/Migrating.md): from a copied `internal/postgrestest`, with measurements.
- [docs/design.md](docs/design.md): the design, decisions P1 onwards.

Requires macOS or Linux for the embedded server (not as root); `POSTGRESTEST_URL` works anywhere.
