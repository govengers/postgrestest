package postgrestest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

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

// unusedTemplateAge is how long a fingerprinted template stays on a shared server after its last
// use: long enough for every run that may still copy it, short enough that templates of edited
// migrations do not pile up.
const unusedTemplateAge = 24 * time.Hour

// SQLSTATE codes the template handling reacts to.
const (
	invalidCatalogName      = "3D000" // a database that is gone
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
	if err := cloneDatabase(ctx, s.cluster.administration, name, template); err != nil {
		return "", err
	}
	return name, nil
}

// ensureTemplate starts the server and builds the template once; parallel tests wait for it.
func (s *Server) ensureTemplate(ctx context.Context) (string, error) {
	if err := s.cluster.start(ctx); err != nil {
		return "", err
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.template != "" {
		return s.template, nil
	}
	name, err := s.templateName()
	if err != nil {
		return "", err
	}
	if err := s.underBuildLock(ctx, func(connection *pgxpool.Conn) error {
		return s.prepareTemplate(ctx, connection, name)
	}); err != nil {
		return "", err
	}
	if s.fingerprint == "" {
		s.cluster.addProcessTemplate(name)
	}
	s.template = name
	return name, nil
}

// prepareTemplate builds the template unless the server has it already, and for a fingerprinted
// one records the use and drops the package's templates nobody used for a day: built for
// migrations the package no longer has. A run of the same package with other migrations, in this
// checkout at the same time, keeps its template.
func (s *Server) prepareTemplate(ctx context.Context, connection *pgxpool.Conn, name string) error {
	if err := s.buildTemplate(ctx, connection, name); err != nil || s.fingerprint == "" {
		return err
	}
	used := time.Now().UTC()
	if _, err := connection.Exec(ctx, "COMMENT ON DATABASE "+identifier(name)+" IS "+literal(used.Format(time.RFC3339))); err != nil {
		return fmt.Errorf("record the use of the template: %w", err)
	}
	return s.dropUnusedTemplates(ctx, connection, used.Add(-unusedTemplateAge))
}

// underBuildLock runs build holding the package's advisory lock on the server, so test binaries
// of one package that share a server build and drop their templates one at a time.
func (s *Server) underBuildLock(ctx context.Context, build func(connection *pgxpool.Conn) error) error {
	connection, err := s.cluster.administration.Acquire(ctx)
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
	return nil
}

func (s *Server) runMigrate(ctx context.Context, database string) error {
	if s.migrate == nil {
		return nil
	}
	connection, err := pgx.Connect(ctx, s.cluster.databaseURL(database))
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

// dropUnusedTemplates drops the package's fingerprinted templates last used before the cutoff,
// and those without a readable record of their use.
func (s *Server) dropUnusedTemplates(ctx context.Context, connection *pgxpool.Conn, cutoff time.Time) error {
	rows, err := connection.Query(ctx, `SELECT datname, coalesce(shobj_description(oid, 'pg_database'), '') FROM pg_database
		WHERE datistemplate AND starts_with(datname, $1)`, s.name(fingerprintKind))
	if err != nil {
		return fmt.Errorf("list the templates: %w", err)
	}
	type template struct{ name, used string }
	templates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (template, error) {
		var found template
		return found, row.Scan(&found.name, &found.used)
	})
	if err != nil {
		return fmt.Errorf("list the templates: %w", err)
	}
	for _, found := range templates {
		if used, err := time.Parse(time.RFC3339, found.used); err == nil && !used.Before(cutoff) {
			continue
		}
		if err := dropTemplate(ctx, connection, found.name); err != nil {
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

// literal quotes a string constant for the statements that take no parameters.
func literal(text string) string { return "'" + strings.ReplaceAll(text, "'", "''") + "'" }

func hasCode(err error, code string) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == code
}
