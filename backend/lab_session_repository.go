package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type LabSessionRepository struct {
	db *sql.DB
}

func NewLabSessionRepository(db *sql.DB) (*LabSessionRepository, error) {
	if db == nil {
		return nil, ErrDatabaseOperation
	}
	return &LabSessionRepository{db: db}, nil
}

func (repository *LabSessionRepository) Create(ctx context.Context, session LabSessionRecord) error {
	if err := validateLabSessionRecord(session); err != nil {
		return err
	}

	const query = `
INSERT INTO lab_sessions (
    id, user_id, lab_id, state, runtime_namespace, runtime_pod_name,
    runtime_service_name, created_at, updated_at, started_at, stopped_at, failure_code
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`
	_, err := repository.db.ExecContext(
		ctx,
		query,
		session.ID,
		session.UserID,
		session.LabID,
		session.State,
		session.RuntimeNamespace,
		session.RuntimePodName,
		session.RuntimeServiceName,
		session.CreatedAt,
		session.UpdatedAt,
		session.StartedAt,
		session.StoppedAt,
		session.FailureCode,
	)
	if err != nil {
		return fmt.Errorf("%w: create lab session", ErrDatabaseOperation)
	}
	return nil
}

func (repository *LabSessionRepository) FindByID(ctx context.Context, id uuid.UUID) (LabSessionRecord, error) {
	if id == uuid.Nil {
		return LabSessionRecord{}, ErrInvalidInput
	}
	return repository.findOne(ctx, sessionByIDQuery, id)
}

func (repository *LabSessionRepository) FindByIDAndUserID(ctx context.Context, id, userID uuid.UUID) (LabSessionRecord, error) {
	if id == uuid.Nil || userID == uuid.Nil {
		return LabSessionRecord{}, ErrInvalidInput
	}
	return repository.findOne(ctx, sessionByIDAndUserIDQuery, id, userID)
}

func (repository *LabSessionRepository) ListByUserID(ctx context.Context, userID uuid.UUID, limit, offset int) ([]LabSessionRecord, error) {
	if userID == uuid.Nil || limit < 0 || offset < 0 || limit > maxSessionPageSize {
		return nil, ErrInvalidInput
	}
	if limit == 0 {
		limit = defaultSessionPageSize
	}

	rows, err := repository.db.QueryContext(ctx, listSessionsByUserIDQuery, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("%w: list lab sessions", ErrDatabaseOperation)
	}
	defer rows.Close()

	sessions := make([]LabSessionRecord, 0, limit)
	for rows.Next() {
		session, scanErr := scanLabSession(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("%w: list lab sessions", ErrDatabaseOperation)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: list lab sessions", ErrDatabaseOperation)
	}
	return sessions, nil
}

func (repository *LabSessionRepository) ListByStates(ctx context.Context, states []SessionState, limit, offset int) ([]LabSessionRecord, error) {
	if len(states) == 0 || limit < 0 || offset < 0 || limit > maxSessionPageSize {
		return nil, ErrInvalidInput
	}
	if limit == 0 {
		limit = defaultSessionPageSize
	}
	for _, state := range states {
		if !validSessionState(state) {
			return nil, ErrInvalidInput
		}
	}

	placeholders := make([]string, len(states))
	arguments := make([]any, 0, len(states)+2)
	for index, state := range states {
		placeholders[index] = fmt.Sprintf("$%d", index+1)
		arguments = append(arguments, state)
	}
	arguments = append(arguments, limit, offset)
	query := fmt.Sprintf(
		"SELECT %s FROM lab_sessions WHERE state IN (%s) ORDER BY updated_at ASC, id ASC LIMIT $%d OFFSET $%d",
		sessionColumns,
		strings.Join(placeholders, ", "),
		len(states)+1,
		len(states)+2,
	)
	rows, err := repository.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("%w: list reconciliation sessions", ErrDatabaseOperation)
	}
	defer rows.Close()

	sessions := make([]LabSessionRecord, 0, limit)
	for rows.Next() {
		session, scanErr := scanLabSession(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("%w: list reconciliation sessions", ErrDatabaseOperation)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: list reconciliation sessions", ErrDatabaseOperation)
	}
	return sessions, nil
}

func (repository *LabSessionRepository) UpdateState(ctx context.Context, id uuid.UUID, next SessionState, updatedAt time.Time) error {
	if id == uuid.Nil || !validSessionState(next) || updatedAt.IsZero() {
		return ErrInvalidInput
	}
	if err := repository.checkTransition(ctx, id, next); err != nil {
		return err
	}

	const query = `
UPDATE lab_sessions
SET state = $2,
    updated_at = $3,
    started_at = CASE WHEN $2 = 'READY' AND started_at IS NULL THEN $3 ELSE started_at END
WHERE id = $1`
	return repository.execFound(ctx, query, id, next, updatedAt)
}

func (repository *LabSessionRepository) UpdateStateIfCurrent(ctx context.Context, id uuid.UUID, expected, next SessionState, updatedAt time.Time) error {
	if id == uuid.Nil || !validSessionState(expected) || !validSessionState(next) || updatedAt.IsZero() {
		return ErrInvalidInput
	}
	if !validStateTransition(expected, next) {
		return ErrInvalidStateTransition
	}

	const query = `
UPDATE lab_sessions
SET state = $3,
    updated_at = $4,
    started_at = CASE WHEN $3 = 'READY' AND started_at IS NULL THEN $4 ELSE started_at END
WHERE id = $1 AND state = $2`
	return repository.execFound(ctx, query, id, expected, next, updatedAt)
}

func (repository *LabSessionRepository) UpdateRuntimeReferences(ctx context.Context, id uuid.UUID, namespace, podName, serviceName *string, updatedAt time.Time) error {
	if id == uuid.Nil || updatedAt.IsZero() {
		return ErrInvalidInput
	}
	if err := validateOptionalRuntimeReference(namespace, podName, serviceName); err != nil {
		return err
	}

	const query = `
UPDATE lab_sessions
SET runtime_namespace = $2,
    runtime_pod_name = $3,
    runtime_service_name = $4,
    updated_at = $5
WHERE id = $1`
	return repository.execFound(ctx, query, id, namespace, podName, serviceName, updatedAt)
}

func (repository *LabSessionRepository) UpdateFailure(ctx context.Context, id uuid.UUID, failureCode string, updatedAt time.Time) error {
	if id == uuid.Nil || failureCode == "" || updatedAt.IsZero() {
		return ErrInvalidInput
	}
	if err := repository.checkTransition(ctx, id, SessionFailed); err != nil {
		return err
	}

	const query = `
UPDATE lab_sessions
SET state = 'FAILED', failure_code = $2, updated_at = $3
WHERE id = $1`
	return repository.execFound(ctx, query, id, failureCode, updatedAt)
}

func (repository *LabSessionRepository) UpdateFailureIfCurrent(ctx context.Context, id uuid.UUID, expected SessionState, failureCode string, updatedAt time.Time) error {
	if id == uuid.Nil || !validSessionState(expected) || failureCode == "" || updatedAt.IsZero() {
		return ErrInvalidInput
	}
	if !validStateTransition(expected, SessionFailed) {
		return ErrInvalidStateTransition
	}

	const query = `
UPDATE lab_sessions
SET state = 'FAILED', failure_code = $3, updated_at = $4
WHERE id = $1 AND state = $2`
	return repository.execFound(ctx, query, id, expected, failureCode, updatedAt)
}

func (repository *LabSessionRepository) UpdateStopped(ctx context.Context, id uuid.UUID, stoppedAt, updatedAt time.Time) error {
	if id == uuid.Nil || stoppedAt.IsZero() || updatedAt.IsZero() {
		return ErrInvalidInput
	}
	if err := repository.checkTransition(ctx, id, SessionStopped); err != nil {
		return err
	}

	const query = `
UPDATE lab_sessions
SET state = 'STOPPED', stopped_at = $2, updated_at = $3
WHERE id = $1`
	return repository.execFound(ctx, query, id, stoppedAt, updatedAt)
}

func (repository *LabSessionRepository) UpdateStoppedIfCurrent(ctx context.Context, id uuid.UUID, expected SessionState, stoppedAt, updatedAt time.Time) error {
	if id == uuid.Nil || !validSessionState(expected) || stoppedAt.IsZero() || updatedAt.IsZero() {
		return ErrInvalidInput
	}
	if !validStateTransition(expected, SessionStopped) {
		return ErrInvalidStateTransition
	}

	const query = `
UPDATE lab_sessions
SET state = 'STOPPED', stopped_at = $3, updated_at = $4
WHERE id = $1 AND state = $2`
	return repository.execFound(ctx, query, id, expected, stoppedAt, updatedAt)
}

const sessionColumns = `id, user_id, lab_id, state, runtime_namespace, runtime_pod_name, runtime_service_name, created_at, updated_at, started_at, stopped_at, failure_code`

const sessionByIDQuery = `SELECT ` + sessionColumns + ` FROM lab_sessions WHERE id = $1`
const sessionByIDAndUserIDQuery = `SELECT ` + sessionColumns + ` FROM lab_sessions WHERE id = $1 AND user_id = $2`
const listSessionsByUserIDQuery = `SELECT ` + sessionColumns + ` FROM lab_sessions WHERE user_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`

type sessionScanner interface {
	Scan(dest ...any) error
}

func (repository *LabSessionRepository) findOne(ctx context.Context, query string, args ...any) (LabSessionRecord, error) {
	row := repository.db.QueryRowContext(ctx, query, args...)
	session, err := scanLabSession(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return LabSessionRecord{}, ErrLabSessionNotFound
		}
		return LabSessionRecord{}, fmt.Errorf("%w: find lab session", ErrDatabaseOperation)
	}
	return session, nil
}

func scanLabSession(scanner sessionScanner) (LabSessionRecord, error) {
	var session LabSessionRecord
	var runtimeNamespace, runtimePodName, runtimeServiceName sql.NullString
	var startedAt, stoppedAt sql.NullTime
	var failureCode sql.NullString
	if err := scanner.Scan(
		&session.ID,
		&session.UserID,
		&session.LabID,
		&session.State,
		&runtimeNamespace,
		&runtimePodName,
		&runtimeServiceName,
		&session.CreatedAt,
		&session.UpdatedAt,
		&startedAt,
		&stoppedAt,
		&failureCode,
	); err != nil {
		return LabSessionRecord{}, err
	}
	if runtimeNamespace.Valid {
		session.RuntimeNamespace = &runtimeNamespace.String
	}
	if runtimePodName.Valid {
		session.RuntimePodName = &runtimePodName.String
	}
	if runtimeServiceName.Valid {
		session.RuntimeServiceName = &runtimeServiceName.String
	}
	if startedAt.Valid {
		session.StartedAt = &startedAt.Time
	}
	if stoppedAt.Valid {
		session.StoppedAt = &stoppedAt.Time
	}
	if failureCode.Valid {
		session.FailureCode = &failureCode.String
	}
	return session, nil
}

func (repository *LabSessionRepository) checkTransition(ctx context.Context, id uuid.UUID, next SessionState) error {
	var current SessionState
	err := repository.db.QueryRowContext(ctx, `SELECT state FROM lab_sessions WHERE id = $1`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLabSessionNotFound
	}
	if err != nil {
		return fmt.Errorf("%w: read lab session state", ErrDatabaseOperation)
	}
	if !validStateTransition(current, next) {
		return ErrInvalidStateTransition
	}
	return nil
}

func (repository *LabSessionRepository) execFound(ctx context.Context, query string, args ...any) error {
	result, err := repository.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%w: update lab session", ErrDatabaseOperation)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%w: update lab session", ErrDatabaseOperation)
	}
	if rows == 0 {
		return ErrLabSessionNotFound
	}
	return nil
}
