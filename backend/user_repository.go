package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type UserRepository interface {
	Create(ctx context.Context, user User) error
	FindByEmail(ctx context.Context, email string) (User, error)
	FindByID(ctx context.Context, id uuid.UUID) (User, error)
	UpdateLastLogin(ctx context.Context, id uuid.UUID, lastLoginAt time.Time) error
}

type PostgresUserRepository struct {
	db *sql.DB
}

func NewPostgresUserRepository(db *sql.DB) (*PostgresUserRepository, error) {
	if db == nil {
		return nil, ErrDatabaseOperation
	}
	return &PostgresUserRepository{db: db}, nil
}

func (repository *PostgresUserRepository) Create(ctx context.Context, user User) error {
	normalizedEmail, err := NormalizeEmail(user.Email)
	if err != nil {
		return err
	}
	user.Email = normalizedEmail
	if err := validateUser(user); err != nil {
		return err
	}

	const query = `
INSERT INTO users (
    id, email, password_hash, role, status, created_at, updated_at, last_login_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

	_, err = repository.db.ExecContext(
		ctx,
		query,
		user.ID,
		user.Email,
		user.PasswordHash,
		user.Role,
		user.Status,
		user.CreatedAt,
		user.UpdatedAt,
		user.LastLoginAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateEmail
		}
		return fmt.Errorf("%w: create user", ErrDatabaseOperation)
	}
	return nil
}

func (repository *PostgresUserRepository) FindByEmail(ctx context.Context, email string) (User, error) {
	normalized, err := NormalizeEmail(email)
	if err != nil {
		return User{}, err
	}

	const query = `
SELECT id, email, password_hash, role, status, created_at, updated_at, last_login_at
FROM users
WHERE email = $1`
	return repository.findOne(ctx, query, normalized)
}

func (repository *PostgresUserRepository) FindByID(ctx context.Context, id uuid.UUID) (User, error) {
	if id == uuid.Nil {
		return User{}, ErrInvalidInput
	}

	const query = `
SELECT id, email, password_hash, role, status, created_at, updated_at, last_login_at
FROM users
WHERE id = $1`
	return repository.findOne(ctx, query, id)
}

func (repository *PostgresUserRepository) UpdateLastLogin(ctx context.Context, id uuid.UUID, lastLoginAt time.Time) error {
	if id == uuid.Nil || lastLoginAt.IsZero() {
		return ErrInvalidInput
	}

	const query = `UPDATE users SET last_login_at = $1, updated_at = $1 WHERE id = $2`
	result, err := repository.db.ExecContext(ctx, query, lastLoginAt, id)
	if err != nil {
		return fmt.Errorf("%w: update last login", ErrDatabaseOperation)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%w: update last login", ErrDatabaseOperation)
	}
	if rows == 0 {
		return ErrUserNotFound
	}
	return nil
}

func (repository *PostgresUserRepository) findOne(ctx context.Context, query string, argument any) (User, error) {
	var user User
	var lastLoginAt sql.NullTime
	row := repository.db.QueryRowContext(ctx, query, argument)
	if err := row.Scan(
		&user.ID,
		&user.Email,
		&user.PasswordHash,
		&user.Role,
		&user.Status,
		&user.CreatedAt,
		&user.UpdatedAt,
		&lastLoginAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrUserNotFound
		}
		return User{}, fmt.Errorf("%w: find user", ErrDatabaseOperation)
	}
	if lastLoginAt.Valid {
		user.LastLoginAt = &lastLoginAt.Time
	}
	return user, nil
}

func isUniqueViolation(err error) bool {
	var sqlStateError interface{ SQLState() string }
	return errors.As(err, &sqlStateError) && sqlStateError.SQLState() == "23505"
}
