# Migrating from a copied harness

The govengers repos carry copies of one embedded-PostgreSQL harness: `internal/postgrestest` in jobs, auth, aikido (`postgresadapter/internal/postgrestest`), wide-event-logger and events, and the `TestMain` of livewire's `postgresadapter`, `query` and `test/e2e` packages. They differ in where they keep the binaries (the OS temp directory or `~/.embedded-postgres-go`), whether they untar with the system tar, and how they sweep orphans. All of them give each test a schema with the migrations applied per test, on an embedded server on a TCP port.

## Steps

1. `go get github.com/govengers/postgrestest@<version>` once it is released (until then nothing to do: this page is the plan).
2. Replace the harness. Either use the library in the test files directly (see [Usage](Usage.md)), or keep the package name and make `internal/postgrestest` a thin wrapper, which leaves every test file as it is. For jobs, whose harness offers `Main`, `New` (MigrationV1 and V2) and `NewMigrated(t, migrations...)`:

   ```go
   package postgrestest

   var (
   	server = postgrestest.New(&postgrestest.Options{
   		Migrate:     postgrestest.Statements(postgresadapter.MigrationV1, postgresadapter.MigrationV2),
   		Fingerprint: postgresadapter.MigrationV1 + postgresadapter.MigrationV2,
   	})
   	others sync.Map // the joined migrations of NewMigrated -> *postgrestest.Server
   )

   func Main(m *testing.M) { goleak.VerifyTestMain(server.Main(m)) }

   func New(t *testing.T) *pgxpool.Pool { return server.Pool(t) }

   func NewMigrated(t *testing.T, migrations ...string) *pgxpool.Pool {
   	key := strings.Join(migrations, "\x00")
   	other, _ := others.LoadOrStore(key, server.WithTemplate(&postgrestest.Options{Migrate: postgrestest.Statements(migrations...), Fingerprint: key}))
   	return other.(*postgrestest.Server).Pool(t)
   }
   ```

   Delete the harness's own internal test (the orphan sweep is tested here now).
3. Check what changes for the tests:
   - A database per test instead of a schema per test: tables are in `public`, `search_path` is the default. `pool.Config().ConnString()` still reaches the test's database, for processes a test starts.
   - The URL is a Unix socket (`postgres://postgres@/<database>?host=<directory>`), not `localhost:<port>`. A test that builds its own DSN from host and port must take the host as a directory.
   - `ALTER DATABASE ... SET` in a migration does not reach the copies; set it on the pool.
   - Tests that applied extra statements through the harness's varargs (auth's password storage tests) run them on their pool, or use `WithTemplate`.
   - Polls of `pg_locks` or `pg_stat_activity` must filter on `current_database()`: every test database shares the server (as every schema did) and copies of one template share table OIDs.
4. justfile: drop `/internal/postgrestest/` from `coverage_excluded` if the wrapper is gone, or keep it for the wrapper. Optional: run the unit tests through the launcher (see [Launcher](Launcher.md)).
5. `.govengers.yml`: the DES-003 exception for the package variable stays, with the reason that it now holds the `postgrestest.Server`.
6. CI: nothing. The govengers `go-library.yml` already caches `~/.embedded-postgres-go`, where the unpacked binaries now live.
7. Leftovers on developer machines: the old harnesses' unpacked binaries in the OS temp directory (`govengers-*-postgres-18.3.0`) and `~/.embedded-postgres-go/govengers-*-postgres-18.3.0` and `auth-instances` can be deleted once no checkout uses them.

## Measurements

M-series Mac (18 cores), macOS, Go 1.27.4, PostgreSQL 18.3, 16.11.2026, other agents' workloads running alongside. CI numbers come from the first CI runs.

| What | Old harnesses | postgrestest |
|------|---------------|--------------|
| Start from unpacked binaries, per test binary | 0.48 to 0.51 s (embedded-postgres `Start`) | 0.43 to 0.51 s (`initdb --no-sync` + `pg_ctl start`), stop 0.15 s |
| Cold start in a `-race` binary, archive cached, binaries not unpacked | 26.9 s (pure-Go xz) | 5.4 s (system tar) |
| Cold start, nothing cached (download) | the same 27 s plus the download | 4.7 s including the download |
| Where the binaries live | OS temp dir (jobs, events, wide-event-logger, livewire) or `~/.embedded-postgres-go` (auth, aikido) | `~/.embedded-postgres-go/postgrestest`, restored by CI |
| Per test | schema plus migrations per test | copy 19.5 ms plus drop 9.3 ms (7.8 MB template, `STRATEGY FILE_COPY`) |

Parallel tests in several packages at once (P15): six packages of 40 `t.Parallel` tests, each `Pool(t)` plus 20 inserts, `go test -count=1 ./...`, same Mac, load average 4 to 16 from other workloads:

| Run | Copies and drops unbounded | At most two per test binary |
|-----|----------------------------|------------------------------|
| A server per test binary | 60.5 s, 76.7 s | 6.3 s, 6.9 s |
| Through the launcher | 59 s (review measurement) | 7 s, 7 s |
| One package of the six | - | 2.2 s |

Unbounded, each binary ran up to as many `CREATE DATABASE` / `DROP DATABASE` statements at once as the machine has cores; each forces checkpoints, and the review saw 18 packages through the launcher fail with drop timeouts. Not measured on Linux.

govengers/jobs at `cc58bc6`, `go test -race -count=1 ./...`, wall clock, switched locally to the wrapper above (not committed to jobs):

| Run | Before | After |
|-----|--------|-------|
| Warm (binaries unpacked) | 14.7 s (three runs: 14.70, 14.74, 14.83) | 14.3 to 16.7 s (14.32, 14.67, 16.73) |
| Cold binaries (no unpacked copy, archive cached) | 41.5 s | 17.0 s |
| Through the launcher, warm | - | 14.8 s, 15.1 s |
| Second plain run without `-count=1` | every package `(cached)` | every package `(cached)` |

The warm wall clock is the root `jobs` package (14 s of its own timing-based tests), not PostgreSQL: the PostgreSQL packages themselves take the same time (postgresadapter 4.9 s before and after, 4.4 s through the launcher; jobstest 1.95 s, 2.0 s, 1.45 s). The gain is the cold path, which every fresh CI runner of a repo whose harness keeps its binaries in the temp directory pays on every run, and one fewer harness copy to fix.
