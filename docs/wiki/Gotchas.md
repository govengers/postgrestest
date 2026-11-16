# Gotchas

- **Call `Main` from `TestMain`.** Without it the embedded server outlives the test binary until the next start sweeps it.
- **Close what Migrate opens.** A connection left on the template fails the build after PostgreSQL waits up to 5 s for it.
- **`ALTER DATABASE ... SET` in a migration does not reach the test databases.** PostgreSQL does not copy database settings; set them on the pool (`RuntimeParams`) or per session.
- **Do not connect to the template.** It refuses connections (`ALLOW_CONNECTIONS false`); a session there would make every copy fail.
- **The URL is a Unix socket** (`host=/path&port=5432` in the query). pgx and libpq handle it; a tool that only speaks TCP does not. When the home directory is so deep that the socket path would pass the platform's limit (107 bytes on Linux, 103 on macOS), the socket moves to a private `/tmp/postgrestest-<digest>`.
- **A Migrate that fails fails every test of the binary at once**, with the same error; fix it and rerun.
- **pg_locks and pg_stat_activity span all databases**, and copies of one template share table OIDs: scope polls to `current_database()`.
- **The fingerprint must change when the migrations change** on a long-lived server; use the SQL itself or `FingerprintFS`. With an embedded server every run builds afresh anyway.
- **A server that outlives the test binary** (`POSTGRESTEST_URL` pointing at a devdb) keeps the test databases of crashed runs; postgrestest cannot tell them from those of a live run.
- **Not as root**: PostgreSQL refuses to run as root, so the embedded server does too. CI runners are fine.
- **Start the launcher the same way every time** (`go run` changes `PATH`, an input of the cache).
