# postgrestest

Fast PostgreSQL for Go tests: a real PostgreSQL 18 per test binary or per test run, and a database of its own for every test, copied from a template your migrations built once.

## Pages

- [Usage](Usage.md): the Server, templates, fingerprints, pools, goose.
- [Launcher](Launcher.md): one server for a whole `go test ./...` run.
- [Test cache](Test-Cache.md): which inputs `go test` sees, and how the results stay cached.
- [Migrating](Migrating.md): from a copied `internal/postgrestest` harness, with measurements.
- [Gotchas](Gotchas.md): the rules that are easy to get wrong.
- [Design](../design.md): decisions P1 onwards.

## Which piece for which need

| Need | Use |
|------|-----|
| A database per test with the package's tables | `postgrestest.New(&Options{Migrate, Fingerprint})`, `Pool(t)` or `Database(t)` |
| `TestMain` with goleak | `goleak.VerifyTestMain(postgres.Main(m))` |
| SQL texts as migrations | `postgrestest.Statements(MigrationV1, MigrationV2)` |
| goose migrations in an `embed.FS` | a `Migrate` that runs goose, `FingerprintFS(migrations)` |
| Some tests need another schema (an upgrade test) | `postgres.WithTemplate(&Options{...})` |
| One server for all packages of a run | `go run github.com/govengers/postgrestest/cmd/postgrestest go test ./...` |
| A server someone else runs | `POSTGRESTEST_URL=postgres://...` |
