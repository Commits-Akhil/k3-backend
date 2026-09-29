package main

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPostgresUserRepositoryCreateMapsDuplicateEmail(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	repository, err := NewPostgresUserRepository(db)
	if err != nil {
		t.Fatalf("NewPostgresUserRepository returned error: %v", err)
	}
	user := testUser()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO users")).
		WithArgs(user.ID, user.Email, user.PasswordHash, user.Role, user.Status, user.CreatedAt, user.UpdatedAt, user.LastLoginAt).
		WillReturnError(&pgconn.PgError{Code: "23505"})

	err = repository.Create(context.Background(), user)
	if !errors.Is(err, ErrDuplicateEmail) {
		t.Fatalf("Create error = %v, want ErrDuplicateEmail", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestPostgresUserRepositoryFindByEmailNormalizesAndReturnsUser(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	repository, err := NewPostgresUserRepository(db)
	if err != nil {
		t.Fatalf("NewPostgresUserRepository returned error: %v", err)
	}
	user := testUser()
	columns := []string{"id", "email", "password_hash", "role", "status", "created_at", "updated_at", "last_login_at"}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, email, password_hash, role, status, created_at, updated_at, last_login_at\nFROM users\nWHERE email = $1")).
		WithArgs(user.Email).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(user.ID.String(), user.Email, user.PasswordHash, string(user.Role), string(user.Status), user.CreatedAt, user.UpdatedAt, nil))

	got, err := repository.FindByEmail(context.Background(), "  STUDENT@EXAMPLE.COM ")
	if err != nil {
		t.Fatalf("FindByEmail returned error: %v", err)
	}
	if got.ID != user.ID || got.Email != user.Email || got.Role != user.Role || got.Status != user.Status {
		t.Fatalf("FindByEmail returned %#v, want %#v", got, user)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestPostgresUserRepositoryDistinguishesNotFoundAndDatabaseFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()
	repository, err := NewPostgresUserRepository(db)
	if err != nil {
		t.Fatalf("NewPostgresUserRepository returned error: %v", err)
	}
	query := regexp.QuoteMeta("SELECT id, email, password_hash, role, status, created_at, updated_at, last_login_at\nFROM users\nWHERE id = $1")
	id := uuid.New()
	mock.ExpectQuery(query).WithArgs(id).WillReturnError(sql.ErrNoRows)
	if _, err := repository.FindByID(context.Background(), id); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("missing user error = %v, want ErrUserNotFound", err)
	}
	mock.ExpectQuery(query).WithArgs(id).WillReturnError(errors.New("database contains private details"))
	err = nil
	_, err = repository.FindByID(context.Background(), id)
	if !errors.Is(err, ErrDatabaseOperation) || strings.Contains(err.Error(), "private details") {
		t.Fatalf("database error = %v, want sanitized ErrDatabaseOperation", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func testUser() User {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	return User{
		ID:           uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Email:        "student@example.com",
		PasswordHash: "$2a$12$test-hash-value-for-repository-tests",
		Role:         UserRoleStudent,
		Status:       UserStatusActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}
