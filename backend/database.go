package main

import (
	"context"
	"database/sql"
	"errors"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func OpenDatabase(config DatabaseConfig) (*sql.DB, error) {
	if err := validateDatabaseConfig(config); err != nil {
		return nil, err
	}

	db, err := sql.Open("pgx", config.DSN)
	if err != nil {
		return nil, errors.New("open database connection failed")
	}

	db.SetMaxOpenConns(config.MaxOpenConns)
	db.SetMaxIdleConns(config.MaxIdleConns)
	db.SetConnMaxLifetime(config.ConnMaxLifetime)
	db.SetConnMaxIdleTime(config.ConnMaxIdleTime)
	return db, nil
}

func CheckDatabase(ctx context.Context, db *sql.DB, timeout time.Duration) error {
	if db == nil {
		return errors.New("database connection is required")
	}
	if timeout <= 0 {
		return errors.New("database check timeout must be positive")
	}

	checkContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := db.PingContext(checkContext); err != nil {
		return errors.New("database connectivity check failed")
	}
	return nil
}

func validateDatabaseConfig(config DatabaseConfig) error {
	if err := validateDatabaseDSN(config.DSN); err != nil {
		return err
	}
	if config.MaxOpenConns <= 0 || config.MaxIdleConns <= 0 || config.MaxIdleConns > config.MaxOpenConns {
		return errors.New("database connection pool limits are invalid")
	}
	if config.ConnMaxLifetime <= 0 || config.ConnMaxIdleTime <= 0 || config.PingTimeout <= 0 {
		return errors.New("database connection timeouts are invalid")
	}
	return nil
}
