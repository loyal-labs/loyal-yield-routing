package db

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"time"
)

// OpenLazy returns a pool that dials on first use, so a database that is down
// fails the queries that need it instead of the process that holds the pool.
func OpenLazy(ctx context.Context, dsn string, maxConnections int32) (*pgxpool.Pool, error) {
	if dsn == "" || maxConnections < 1 {
		return nil, errors.New("missing bounded database configuration")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid database configuration")
	}
	config.MaxConns = maxConnections
	return pgxpool.NewWithConfig(ctx, config)
}

func Open(ctx context.Context, dsn string, maxConnections int32) (*pgxpool.Pool, error) {
	pool, err := OpenLazy(ctx, dsn, maxConnections)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// WithTx owns commit/rollback; a canceled begin context does not clean up a pgx tx.
func WithTx(ctx context.Context, pool *pgxpool.Pool, options pgx.TxOptions, fn func(pgx.Tx) error) (err error) {
	if pool == nil || fn == nil {
		return errors.New("missing transaction dependency")
	}
	tx, err := pool.BeginTx(ctx, options)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
