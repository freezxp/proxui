// Package postgres provides the database connection pool and migration runner.
package postgres

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver used by the migrator
	"github.com/pressly/goose/v3"
	"github.com/rs/zerolog"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationLockID namespaces the advisory lock that serializes migrations, so
// several API instances starting at once cannot race each other.
const migrationLockID int64 = 8_147_230_001

// Pool wraps pgxpool so the transport layer can depend on a small interface.
type Pool struct{ *pgxpool.Pool }

// Connect opens the connection pool and verifies it with a ping.
func Connect(ctx context.Context, dsn string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 20
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Pool{pool}, nil
}

// Ping implements the readiness Checker contract.
func (p *Pool) Ping(ctx context.Context) error { return p.Pool.Ping(ctx) }

// Migrate applies all pending migrations under an advisory lock.
func Migrate(ctx context.Context, dsn string, log zerolog.Logger) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open migration connection: %w", err)
	}
	defer db.Close()

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		if _, err := conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockID); err != nil {
			log.Error().Err(err).Msg("release migration lock")
		}
	}()

	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(gooseLogger{log})
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	version, err := goose.GetDBVersion(db)
	if err == nil {
		log.Info().Int64("schema_version", version).Msg("migrations applied")
	}
	return nil
}

// gooseLogger routes goose output into the structured logger.
type gooseLogger struct{ log zerolog.Logger }

func (g gooseLogger) Fatalf(format string, v ...any) { g.log.Fatal().Msgf(format, v...) }
func (g gooseLogger) Printf(format string, v ...any) { g.log.Info().Msgf(format, v...) }
