package postgrestest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Database names: postgrestest_<scope>_<kind><suffix>, all within PostgreSQL's 63 bytes. The scope
// keeps packages and checkouts apart on a shared server; the kind tells a fingerprinted template
// (which another run may reuse) from one built for one process and from a test's database.
const (
	namePrefix          = "postgrestest_"
	fingerprintKind     = "f"
	processTemplateKind = "p"
	databaseKind        = "d"
	buildSuffix         = "build"
)

// SQLSTATE codes the template handling reacts to.
const (
	invalidCatalogName      = "3D000" // CREATE DATABASE ... TEMPLATE of a template that is gone
	objectInUse             = "55006" // a database still has sessions
	advisoryLockNamespace   = "postgrestest template "
	randomSuffixLength      = 8
	fingerprintDigestLength = 8
)

// Statements returns a Migrate that runs each SQL text in a transaction of its own, in order, as
// goose runs migration files; the texts may hold several statements each.
func Statements(texts ...string) func(ctx context.Context, connection *pgx.Conn) error {
	return func(ctx context.Context, connection *pgx.Conn) error {
		for index, text := range texts {
			if err := pgx.BeginFunc(ctx, connection, func(transaction pgx.Tx) error {
				_, err := transaction.Exec(ctx, text)
				return err
			}); err != nil {
				return fmt.Errorf("statements %d: %w", index+1, err)
			}
		}
		return nil
	}
}

// createDatabase copies the template into a new database and returns its name.
func (s *Server) createDatabase(ctx context.Context) (string, error) {
	template, err := s.ensureTemplate(ctx)
	if err != nil {
		return "", err
	}
	name, err := s.newName(databaseKind)
	if err != nil {
		return "", err
	}
	err = cloneDatabase(ctx, s.administration, name, template)
	if hasCode(err, invalidCatalogName) {
		// a run of the same package with other migrations dropped the template: build it again and
		// copy it while still holding the build lock, so that run cannot drop it in between
		err = s.rebuildAndClone(ctx, name)
	}
	if err != nil {
		return "", err
	}
	return name, nil
}

// ensureTemplate starts the server and builds the template once; parallel tests wait for it.
func (s *Server) ensureTemplate(ctx context.Context) (string, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if err := s.start(ctx); err != nil {
		return "", err
	}
	if s.template != "" {
		return s.template, nil
	}
	name, err := s.templateName()
	if err != nil {
		return "", err
	}
	if err := s.underBuildLock(ctx, func(connection *pgxpool.Conn) error {
		return s.buildTemplate(ctx, connection, name)
	}); err != nil {
		return "", err
	}
	s.template = name
	return name, nil
}

func (s *Server) rebuildAndClone(ctx context.Context, name string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.underBuildLock(ctx, func(connection *pgxpool.Conn) error {
		if err := s.buildTemplate(ctx, connection, s.template); err != nil {
			return err
		}
		return cloneDatabase(ctx, connection, name, s.template)
	})
}

// underBuildLock runs build holding the package's advisory lock on the server, so test binaries
// of one package that share a server build and drop their templates one at a time.
func (s *Server) underBuildLock(ctx context.Context, build func(connection *pgxpool.Conn) error) error {
	connection, err := s.administration.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("connect to the server: %w", err)
	}
	defer connection.Release()
	key := s.lockKey()
	if _, err := connection.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		return fmt.Errorf("take the template lock: %w", err)
	}
	buildError := build(connection)
	if _, err := connection.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", key); err != nil {
		// a session that keeps the lock would block the next build of the package forever
		_ = connection.Hijack().Close(context.WithoutCancel(ctx)) // closing ends the session and its lock; the release error is returned
		return errors.Join(buildError, fmt.Errorf("release the template lock: %w", err))
	}
	return buildError
}

// buildTemplate creates the template unless the server has it. It builds under a scratch name
// and renames it at the end, so no run ever copies a half-migrated template.
func (s *Server) buildTemplate(ctx context.Context, connection *pgxpool.Conn, name string) error {
	if exists, err := databaseExists(ctx, connection, name); err != nil || exists {
		return err
	}
	scratch := s.name(buildSuffix)
	for _, statement := range []string{
		"DROP DATABASE IF EXISTS " + identifier(scratch) + " WITH (FORCE)", // left by a builder that died
		"CREATE DATABASE " + identifier(scratch),
	} {
		if _, err := connection.Exec(ctx, statement); err != nil {
			return fmt.Errorf("create the template: %w", err)
		}
	}
	if err := s.runMigrate(ctx, scratch); err != nil {
		return errors.Join(err, dropDatabase(ctx, connection, scratch))
	}
	if _, err := connection.Exec(ctx, "ALTER DATABASE "+identifier(scratch)+" RENAME TO "+identifier(name)); err != nil {
		if hasCode(err, objectInUse) {
			err = errors.New("the template still has a session after Migrate: close every connection Migrate opens")
		}
		return errors.Join(fmt.Errorf("publish the template: %w", err), dropDatabase(ctx, connection, scratch))
	}
	// IS_TEMPLATE keeps it from being dropped by accident, ALLOW_CONNECTIONS false keeps sessions
	// out that would make every copy fail
	if _, err := connection.Exec(ctx, "ALTER DATABASE "+identifier(name)+" WITH IS_TEMPLATE true ALLOW_CONNECTIONS false"); err != nil {
		return fmt.Errorf("mark the template: %w", err)
	}
	return s.dropOlderTemplates(ctx, connection, name)
}

func (s *Server) runMigrate(ctx context.Context, database string) error {
	if s.migrate == nil {
		return nil
	}
	connection, err := pgx.Connect(ctx, s.databaseURL(database))
	if err != nil {
		return fmt.Errorf("connect to the template: %w", err)
	}
	migrateError := s.migrate(ctx, connection)
	closeError := connection.Close(context.WithoutCancel(ctx))
	if migrateError != nil {
		return fmt.Errorf("migrate the template: %w", migrateError)
	}
	return closeError
}

// dropOlderTemplates drops the fingerprinted templates of the package other than keep: they were
// built for migrations the package no longer has.
func (s *Server) dropOlderTemplates(ctx context.Context, connection *pgxpool.Conn, keep string) error {
	rows, err := connection.Query(ctx, "SELECT datname FROM pg_database WHERE datistemplate AND starts_with(datname, $1) AND datname <> $2",
		s.name(fingerprintKind), keep)
	if err != nil {
		return fmt.Errorf("list older templates: %w", err)
	}
	older, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("list older templates: %w", err)
	}
	for _, template := range older {
		if err := dropTemplate(ctx, connection, template); err != nil {
			return err
		}
	}
	return nil
}

// executor is what the template statements need of a pool or one of its connections.
type executor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func cloneDatabase(ctx context.Context, server executor, name, template string) error {
	if _, err := server.Exec(ctx, "CREATE DATABASE "+identifier(name)+" TEMPLATE "+identifier(template)+" STRATEGY FILE_COPY"); err != nil {
		return fmt.Errorf("copy the template: %w", err)
	}
	return nil
}

func dropTemplate(ctx context.Context, server executor, name string) error {
	if _, err := server.Exec(ctx, "ALTER DATABASE "+identifier(name)+" WITH IS_TEMPLATE false"); err != nil && !hasCode(err, invalidCatalogName) {
		return fmt.Errorf("drop the template %s: %w", name, err)
	}
	return dropDatabase(ctx, server, name)
}

func dropDatabase(ctx context.Context, server executor, name string) error {
	if _, err := server.Exec(ctx, "DROP DATABASE IF EXISTS "+identifier(name)+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop %s: %w", name, err)
	}
	return nil
}

func databaseExists(ctx context.Context, connection *pgxpool.Conn, name string) (bool, error) {
	var exists bool
	if err := connection.QueryRow(ctx, "SELECT EXISTS (SELECT FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		return false, fmt.Errorf("look up the template: %w", err)
	}
	return exists, nil
}

// templateName is the fingerprint's template, or one of this process only without a fingerprint.
func (s *Server) templateName() (string, error) {
	if s.fingerprint == "" {
		return s.newName(processTemplateKind)
	}
	digest := sha256.Sum256([]byte(s.fingerprint))
	return s.name(fingerprintKind + hex.EncodeToString(digest[:fingerprintDigestLength])), nil
}

func (s *Server) newName(kind string) (string, error) {
	suffix := make([]byte, randomSuffixLength)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("random database name: %w", err)
	}
	return s.name(kind + hex.EncodeToString(suffix)), nil
}

func (s *Server) name(suffix string) string { return namePrefix + s.scope + "_" + suffix }

func (s *Server) lockKey() int64 {
	digest := sha256.Sum256([]byte(advisoryLockNamespace + s.scope))
	return int64(binary.BigEndian.Uint64(digest[:8])) //nolint:gosec // G115: any 64 bits make a lock key
}

func identifier(name string) string { return pgx.Identifier{name}.Sanitize() }

func hasCode(err error, code string) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == code
}
