# Launcher

`cmd/postgrestest` starts one PostgreSQL for a whole test run and points every test binary at it:

```just
postgrestest := "go run github.com/govengers/postgrestest/cmd/postgrestest@v0.1.0"

test *flags:
    {{postgrestest}} {{gotestsum}} --format testname -- -race {{flags}} ./...
```

- It runs the command with `POSTGRESTEST_URL` set, stops the server when the command ends and exits with the command's status (2 without a command, 1 when the launcher fails).
- The server's socket lives in `~/.embedded-postgres-go/postgrestest/run-<digest of the working directory>` (or a `/tmp/postgrestest-<digest>` of it for a very deep home directory), so the URL is the same in every run of one checkout and `go test` keeps replaying cached results (see [Test cache](Test-Cache.md)).
- A second launcher in the same checkout while the first runs gets a directory of its own; its results miss the cache once.
- With `POSTGRESTEST_URL` already set, it runs the command against that server and starts nothing.
- Ctrl-C and SIGTERM reach the command; the launcher waits for it and then stops the server. A launcher that is killed leaves its server behind; the next start of any postgrestest server on the machine stops it.
- Packages tested on their own (`go test ./store`) start their own server as before.

What it saves: one `initdb` and start per test binary (about 0.5 s each on a Mac), so with several PostgreSQL packages it is mostly CPU on small CI runners. It does not change what the tests see: every test still gets its own database.
