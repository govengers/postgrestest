# Test cache

`go test` replays the result of a passing package until something that went into it changes: the test binary, an environment variable the binary read (with its value), or a file inside the module that it opened or stat'ed (by size and modification time). Files outside the module root, GOPATH and GOROOT are not checked.

What a package using postgrestest feeds into that key:

| Input | Why | Stable? |
|-------|-----|---------|
| `POSTGRESTEST_URL` | where the server is | unset in plain runs; the launcher's URL depends only on the checkout's path |
| `HOME` | the binaries and server directories live below `~/.embedded-postgres-go` | yes |
| `PATH` | `tar` and `ps` | yes, but `go run` puts GOROOT/bin in front of it for the command it runs: start the launcher the same way every time |
| pgx's `PG*` variables | pgx reads them while parsing a connection string | yes |
| files below `~/.embedded-postgres-go` | binaries, locks, server directories | outside every module, not hashed |
| the embedded server's socket path | random per test binary | never read from the environment, not an input |
| the working directory | names the package's databases (P6) | read with `syscall.Getwd`, not `os.Getwd`, which reads `PWD`: `go test` sets `PWD` to the directory it was started from, so a package would rerun when started from another one. `t.Chdir` reads `PWD` too |

So: two plain runs hit the cache, two launcher runs hit the cache, and switching between the two reruns each package once (the URL changed). Migrations embedded with `embed.FS` or held in Go constants are part of the test binary, so changing them reruns the package; that is the point.

Checked by the end-to-end suite, which runs the fixture packages twice in both modes and expects `(cached)`. To see the inputs yourself: `GODEBUG=gocachetest=1 go test ./pkg` (GODEBUG is an input too, so that run itself misses).
