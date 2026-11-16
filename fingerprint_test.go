package postgrestest

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

func TestFingerprintFSChangesWithAnyFile(t *testing.T) {
	t.Parallel()
	migrations := fstest.MapFS{"001_tasks.sql": {Data: []byte("CREATE TABLE tasks ()")}, "002_users.sql": {Data: []byte("CREATE TABLE users ()")}}
	fingerprint := mustFingerprint(t, migrations)
	require.Equal(t, fingerprint, mustFingerprint(t, fstest.MapFS{"002_users.sql": migrations["002_users.sql"], "001_tasks.sql": migrations["001_tasks.sql"]}))

	for name, changed := range map[string]fstest.MapFS{
		"content": {"001_tasks.sql": {Data: []byte("CREATE TABLE tasks (id int)")}, "002_users.sql": migrations["002_users.sql"]},
		"name":    {"001_task.sql": migrations["001_tasks.sql"], "002_users.sql": migrations["002_users.sql"]},
		"added":   {"001_tasks.sql": migrations["001_tasks.sql"], "002_users.sql": migrations["002_users.sql"], "003_x.sql": {}},
		"moved":   {"001_tasks.sql": {Data: []byte("CREATE TABLE tasks ()CREATE")}, "002_users.sql": {Data: []byte(" TABLE users ()")}},
	} {
		require.NotEqual(t, fingerprint, mustFingerprint(t, changed), name)
	}
}

func TestFingerprintFSReportsUnreadableFiles(t *testing.T) {
	t.Parallel()
	_, err := FingerprintFS(fstest.MapFS{"001.sql": {Mode: fs.ModeSymlink, Data: []byte("missing")}})
	require.ErrorContains(t, err, "fingerprint the migrations")
}

func mustFingerprint(t *testing.T, migrations fs.FS) string {
	t.Helper()
	fingerprint, err := FingerprintFS(migrations)
	require.NoError(t, err)
	return fingerprint
}
