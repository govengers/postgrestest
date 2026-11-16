package postgrestest_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/govengers/postgrestest"
)

// The migrations of the package under test, as a library exports them for goose.
const migrationV1 = `CREATE TABLE tasks (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, title text NOT NULL)`

// A package's test files keep one Server and close it from TestMain; with goleak,
// goleak.VerifyTestMain(postgres.Main(m)) replaces the os.Exit line.
func ExampleNew() {
	postgres := postgrestest.New(&postgrestest.Options{
		Migrate:     postgrestest.Statements(migrationV1),
		Fingerprint: migrationV1,
	})
	testMain := func(m *testing.M) { os.Exit(postgres.Main(m).Run()) }
	_ = testMain
}

// Each test gets a pool on a database of its own with the tables of the template.
func ExampleServer_Pool() {
	var postgres *postgrestest.Server
	testCreate := func(t *testing.T) {
		t.Parallel()
		pool := postgres.Pool(t)
		if _, err := pool.Exec(t.Context(), "INSERT INTO tasks (title) VALUES ('write the tests')"); err != nil {
			t.Fatal(err)
		}
	}
	_ = testCreate
}

// A Migrate of its own runs anything that needs a connection, goose for example, with
// stdlib.OpenDB(*connection.Config()) from github.com/jackc/pgx/v5/stdlib.
func ExampleOptions_migrate() {
	postgres := postgrestest.New(&postgrestest.Options{
		Migrate: func(ctx context.Context, connection *pgx.Conn) error {
			_, err := connection.Exec(ctx, migrationV1)
			return err
		},
		Fingerprint: migrationV1,
	})
	_ = postgres
}
