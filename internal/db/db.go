// Package db opens the Postgres pool and runs versioned migrations.
package db

import (
	"context"
	"embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB bundles the connection pool with the process lifetime.
type DB struct {
	Pool *pgxpool.Pool
}

// Open connects and runs pending migrations.
func Open(ctx context.Context, dsn string) (*DB, error) {
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	if err := migrate(dsn); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrations: %w", err)
	}
	return &DB{Pool: pool}, nil
}

// migrate runs goose migrations from the embedded filesystem using a
// dedicated stdlib connection (goose manages its own advisory locking).
func migrate(dsn string) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse dsn: %w", err)
	}
	sqlDB := stdlib.OpenDB(*cc)
	defer sqlDB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return goose.UpContext(ctx, sqlDB, "migrations")
}

// Close releases the pool.
func (d *DB) Close() { d.Pool.Close() }
