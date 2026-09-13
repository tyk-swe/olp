// Package database owns PostgreSQL connection lifecycle. Feature packages own SQL.
package database

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Configuration(rawURL string, maxConnections int, timeout time.Duration) (*pgxpool.Config, error) {
	c, err := pgxpool.ParseConfig(rawURL)
	if err != nil {
		return nil, errors.New("invalid OLP_DATABASE_URL")
	}
	c.MaxConns = int32(maxConnections)
	c.ConnConfig.ConnectTimeout = timeout
	c.ConnConfig.RuntimeParams["application_name"] = "olp-go"
	return c, nil
}

func Open(ctx context.Context, c *pgxpool.Config) (*pgxpool.Pool, error) {
	pool, err := pgxpool.NewWithConfig(ctx, c)
	if err != nil {
		return nil, errors.New("cannot create PostgreSQL pool")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errors.New("PostgreSQL connection failed")
	}
	return pool, nil
}
