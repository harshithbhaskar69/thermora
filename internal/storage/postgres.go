// Package storage owns database setup. Domain SQL lives alongside its cloud use cases.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"thermora.local/thermora/migrations"
)

func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("invalid database configuration")
	}
	config.MaxConns = 8
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	config.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	config.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "10000"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect PostgreSQL: %w", err)
	}
	return pool, nil
}

func Migrate(ctx context.Context, url string) (err error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.Files)
	if err != nil {
		return err
	}
	if _, err = provider.Up(ctx); err != nil {
		return fmt.Errorf("migrate PostgreSQL: %w", err)
	}
	return nil
}

// ProvisionAppRole grants DML to the runtime identity without schema or role administration.
func ProvisionAppRole(ctx context.Context, ownerURL, password string) error {
	db, err := Open(ctx, ownerURL)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer Rollback(ctx, tx)
	var exists bool
	if err = tx.QueryRow(
		ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='thermora_app')`,
	).Scan(
		&exists,
	); err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(
			ctx,
			`CREATE ROLE thermora_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION`,
		); err != nil {
			return err
		}
	}
	var statement string
	// PostgreSQL quotes the password as a SQL literal; it is never interpolated by application code.
	if err = tx.QueryRow(
		ctx,
		`SELECT format('ALTER ROLE thermora_app PASSWORD %L', $1::text)`,
		password,
	).Scan(
		&statement,
	); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, statement); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `REVOKE CREATE ON SCHEMA public FROM PUBLIC;
		GRANT USAGE ON SCHEMA public TO thermora_app;
		GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA public TO thermora_app;
		REVOKE ALL ON goose_db_version FROM thermora_app;
		GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA public TO thermora_app`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Rollback releases an unfinished transaction, including after request cancellation.
// A committed or already rolled-back transaction needs no further cleanup.
func Rollback(ctx context.Context, tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		slog.ErrorContext(ctx, "transaction rollback failed", "error", err)
	}
}
