//go:build integration

package postgres_test

import (
	"database/sql"
	"io/fs"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/postgres"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/postgres/pgtest"
	"github.com/artemyaroshev1ch/uptime-monitor/migrations"
)

// uniqueViolation is the PostgreSQL error code for a unique constraint violation.
const uniqueViolation = "23505"

func openDB(t *testing.T, url string) *sql.DB {
	t.Helper()
	db, err := postgres.OpenDB(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	err := db.QueryRowContext(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists)
	require.NoError(t, err)
	return exists
}

// TestMigrationsFromScratch applies every migration to an empty database,
// rolls all of them back and applies them again. The second pass proves that
// the Down sections really undo the Up sections.
func TestMigrationsFromScratch(t *testing.T) {
	db := openDB(t, pgtest.Start(t))
	migrator, err := postgres.NewMigrator(db)
	require.NoError(t, err)
	ctx := t.Context()

	files, err := fs.Glob(migrations.FS, "*.sql")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for pass := 1; pass <= 2; pass++ {
		applied, err := migrator.Up(ctx)
		require.NoError(t, err, "pass %d: up", pass)
		assert.Len(t, applied, len(files), "pass %d: every migration is applied", pass)
		for _, table := range []string{"users", "monitors", "checks", "incidents", "notification_channels", "notifications"} {
			assert.True(t, tableExists(t, db, table), "pass %d: %s", pass, table)
		}

		pending, err := migrator.HasPending(ctx)
		require.NoError(t, err)
		assert.False(t, pending, "pass %d: nothing pending after up", pass)

		rolledBack, err := migrator.DownTo(ctx, 0)
		require.NoError(t, err, "pass %d: down", pass)
		assert.Len(t, rolledBack, len(files), "pass %d: every migration is rolled back", pass)
		for _, table := range []string{"users", "monitors", "checks", "incidents", "notification_channels", "notifications"} {
			assert.False(t, tableExists(t, db, table), "pass %d: %s", pass, table)
		}
	}
}

func TestUsersSchema(t *testing.T) {
	db := openDB(t, pgtest.Start(t))
	migrator, err := postgres.NewMigrator(db)
	require.NoError(t, err)
	ctx := t.Context()
	_, err = migrator.Up(ctx)
	require.NoError(t, err)

	const insert = `INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING id, created_at`

	var (
		id        string
		createdAt time.Time
	)
	err = db.QueryRowContext(ctx, insert, "alice@example.com", "hash").Scan(&id, &createdAt)
	require.NoError(t, err)
	assert.NotEmpty(t, id, "id has a database default")
	assert.WithinDuration(t, time.Now(), createdAt, time.Minute, "created_at has a database default")

	_, err = db.ExecContext(ctx, insert, "alice@example.com", "other")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "duplicate email is rejected")
	assert.Equal(t, uniqueViolation, pgErr.Code)
}

// TestConcurrentMigrateUp runs "migrate up" from two processes at once, as
// two overlapping deploys would. The advisory lock makes the second one wait,
// so both succeed and each migration is applied exactly once.
func TestConcurrentMigrateUp(t *testing.T) {
	url := pgtest.Start(t)
	ctx := t.Context()

	const runners = 2
	applied := make([]int, runners)
	g, gctx := errgroup.WithContext(ctx)
	for i := range runners {
		g.Go(func() error {
			// Each runner has its own *sql.DB, i.e. its own session.
			db := openDB(t, url)
			migrator, err := postgres.NewMigrator(db)
			if err != nil {
				return err
			}
			results, err := migrator.Up(gctx)
			applied[i] = len(results)
			return err
		})
	}
	require.NoError(t, g.Wait())

	files, err := fs.Glob(migrations.FS, "*.sql")
	require.NoError(t, err)
	assert.Equal(t, len(files), applied[0]+applied[1], "every migration is applied once in total")

	var rows int
	err = openDB(t, url).QueryRowContext(ctx, `SELECT count(*) FROM goose_db_version WHERE version_id > 0`).Scan(&rows)
	require.NoError(t, err)
	assert.Equal(t, len(files), rows, "no migration is recorded twice")
}

func TestPool(t *testing.T) {
	url := pgtest.Start(t)

	pool, err := postgres.NewPool(t.Context(), postgres.PoolConfig{URL: url, MaxConns: 2})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.NoError(t, pool.Ping(t.Context()))
	assert.Equal(t, int32(2), pool.Stat().MaxConns())
}
