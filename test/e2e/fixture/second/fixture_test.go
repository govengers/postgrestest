//go:build e2efixture

// Package second is a consumer of postgrestest that the end-to-end suite runs with go test, on its own
// and through the launcher; its twin package second/first shares the launcher's server.
package second

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/govengers/postgrestest"
)

const migration = `CREATE TABLE tasks (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, title text NOT NULL)`

var postgres = postgrestest.New(&postgrestest.Options{Migrate: postgrestest.Statements(migration), Fingerprint: migration})

func TestMain(m *testing.M) { goleak.VerifyTestMain(postgres.Main(m)) }

func TestEveryTestHasADatabaseOfItsOwn(t *testing.T) {
	for _, title := range []string{"first", "second", "third", "fourth"} {
		t.Run(title, func(t *testing.T) {
			t.Parallel()
			pool := postgres.Pool(t)
			_, err := pool.Exec(t.Context(), "INSERT INTO tasks (title) VALUES ($1)", title)
			require.NoError(t, err)
			var count int
			require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM tasks").Scan(&count))
			require.Equal(t, 1, count)
		})
	}
}

// The end-to-end suite reads the server's data directory from this line.
func TestReportsItsServer(t *testing.T) {
	var directory string
	require.NoError(t, postgres.Pool(t).QueryRow(t.Context(), "SHOW data_directory").Scan(&directory))
	t.Logf("data_directory=%s", directory)
}
