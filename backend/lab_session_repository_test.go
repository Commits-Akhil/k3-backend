package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

var sessionTestColumns = []string{
	"id", "user_id", "lab_id", "state", "runtime_namespace", "runtime_pod_name",
	"runtime_service_name", "created_at", "updated_at", "started_at", "stopped_at", "failure_code",
}

func newMockLabSessionRepository(t *testing.T) (*LabSessionRepository, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	repository, err := NewLabSessionRepository(db)
	if err != nil {
		db.Close()
		t.Fatalf("NewLabSessionRepository returned error: %v", err)
	}
	return repository, mock, func() { _ = db.Close() }
}

func TestLabSessionRepositoryCreateAndFind(t *testing.T) {
	repository, mock, closeDB := newMockLabSessionRepository(t)
	defer closeDB()
	session := testLabSession()

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO lab_sessions")).
		WithArgs(session.ID, session.UserID, session.LabID, session.State, nil, nil, nil, session.CreatedAt, session.UpdatedAt, nil, nil, nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := repository.Create(context.Background(), session); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta(sessionByIDQuery)).
		WithArgs(session.ID).
		WillReturnRows(sqlmock.NewRows(sessionTestColumns).AddRow(sessionRowValues(session)...))
	got, err := repository.FindByID(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("FindByID returned error: %v", err)
	}
	if got.ID != session.ID || got.UserID != session.UserID || got.LabID != session.LabID || got.State != session.State {
		t.Fatalf("FindByID returned %#v, want %#v", got, session)
	}
	if got.RuntimeNamespace != nil || got.StartedAt != nil || got.FailureCode != nil {
		t.Fatalf("nullable fields = %#v, want nil fields", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestLabSessionRepositoryFindByIDAndUserIDAndNotFound(t *testing.T) {
	repository, mock, closeDB := newMockLabSessionRepository(t)
	defer closeDB()
	session := testLabSession()

	mock.ExpectQuery(regexp.QuoteMeta(sessionByIDAndUserIDQuery)).
		WithArgs(session.ID, session.UserID).
		WillReturnRows(sqlmock.NewRows(sessionTestColumns).AddRow(sessionRowValues(session)...))
	if _, err := repository.FindByIDAndUserID(context.Background(), session.ID, session.UserID); err != nil {
		t.Fatalf("FindByIDAndUserID returned error: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta(sessionByIDQuery)).WithArgs(session.ID).WillReturnError(sql.ErrNoRows)
	if _, err := repository.FindByID(context.Background(), session.ID); !errors.Is(err, ErrLabSessionNotFound) {
		t.Fatalf("missing session error = %v, want ErrLabSessionNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestLabSessionRepositoryListsOrderedPaginatedSessions(t *testing.T) {
	repository, mock, closeDB := newMockLabSessionRepository(t)
	defer closeDB()
	session := testLabSession()
	second := session
	second.ID = uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	second.CreatedAt = second.CreatedAt.Add(time.Hour)

	mock.ExpectQuery(regexp.QuoteMeta(listSessionsByUserIDQuery)).
		WithArgs(session.UserID, 2, 4).
		WillReturnRows(sqlmock.NewRows(sessionTestColumns).
			AddRow(sessionRowValues(second)...).
			AddRow(sessionRowValues(session)...))
	sessions, err := repository.ListByUserID(context.Background(), session.UserID, 2, 4)
	if err != nil {
		t.Fatalf("ListByUserID returned error: %v", err)
	}
	if len(sessions) != 2 || sessions[0].ID != second.ID || sessions[1].ID != session.ID {
		t.Fatalf("listed sessions = %#v, want deterministic created_at order", sessions)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestLabSessionRepositoryUpdatesStateAndTimestamps(t *testing.T) {
	repository, mock, closeDB := newMockLabSessionRepository(t)
	defer closeDB()
	session := testLabSession()
	updatedAt := session.UpdatedAt.Add(time.Minute)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT state FROM lab_sessions WHERE id = $1")).
		WithArgs(session.ID).
		WillReturnRows(sqlmock.NewRows([]string{"state"}).AddRow(string(SessionStarting)))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE lab_sessions")).
		WithArgs(session.ID, SessionReady, updatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repository.UpdateState(context.Background(), session.ID, SessionReady, updatedAt); err != nil {
		t.Fatalf("UpdateState returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestLabSessionRepositoryUpdatesReferencesFailureAndStopped(t *testing.T) {
	repository, mock, closeDB := newMockLabSessionRepository(t)
	defer closeDB()
	session := testLabSession()
	updatedAt := session.UpdatedAt.Add(time.Minute)
	namespace, pod, service := "cyber-labs", "pod-name", "service-name"

	mock.ExpectExec(regexp.QuoteMeta("UPDATE lab_sessions")).
		WithArgs(session.ID, namespace, pod, service, updatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repository.UpdateRuntimeReferences(context.Background(), session.ID, &namespace, &pod, &service, updatedAt); err != nil {
		t.Fatalf("UpdateRuntimeReferences returned error: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT state FROM lab_sessions WHERE id = $1")).
		WithArgs(session.ID).
		WillReturnRows(sqlmock.NewRows([]string{"state"}).AddRow(string(SessionStarting)))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE lab_sessions")).
		WithArgs(session.ID, "startup_failed", updatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repository.UpdateFailure(context.Background(), session.ID, "startup_failed", updatedAt); err != nil {
		t.Fatalf("UpdateFailure returned error: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT state FROM lab_sessions WHERE id = $1")).
		WithArgs(session.ID).
		WillReturnRows(sqlmock.NewRows([]string{"state"}).AddRow(string(SessionStopping)))
	stoppedAt := updatedAt.Add(time.Minute)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE lab_sessions")).
		WithArgs(session.ID, stoppedAt, updatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repository.UpdateStopped(context.Background(), session.ID, stoppedAt, updatedAt); err != nil {
		t.Fatalf("UpdateStopped returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestLabSessionRepositoryRejectsInvalidInputsAndTransitions(t *testing.T) {
	repository, mock, closeDB := newMockLabSessionRepository(t)
	defer closeDB()
	session := testLabSession()
	if err := repository.UpdateState(context.Background(), session.ID, SessionReady, time.Time{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero timestamp error = %v, want ErrInvalidInput", err)
	}
	if _, err := repository.ListByUserID(context.Background(), session.UserID, maxSessionPageSize+1, 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized page error = %v, want ErrInvalidInput", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT state FROM lab_sessions WHERE id = $1")).
		WithArgs(session.ID).
		WillReturnRows(sqlmock.NewRows([]string{"state"}).AddRow(string(SessionStopped)))
	if err := repository.UpdateState(context.Background(), session.ID, SessionReady, session.UpdatedAt); !errors.Is(err, ErrInvalidStateTransition) {
		t.Fatalf("invalid transition error = %v, want ErrInvalidStateTransition", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestLabSessionRepositorySanitizesDatabaseFailures(t *testing.T) {
	repository, mock, closeDB := newMockLabSessionRepository(t)
	defer closeDB()
	session := testLabSession()
	mock.ExpectQuery(regexp.QuoteMeta(sessionByIDQuery)).
		WithArgs(session.ID).
		WillReturnError(errors.New("private DSN and SQL details"))
	_, err := repository.FindByID(context.Background(), session.ID)
	if !errors.Is(err, ErrDatabaseOperation) || strings.Contains(err.Error(), "private DSN") || strings.Contains(err.Error(), "SQL details") {
		t.Fatalf("database error = %v, want sanitized ErrDatabaseOperation", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestLabSessionRepositoryListsReconciliationStatesWithBoundedPagination(t *testing.T) {
	repository, mock, closeDB := newMockLabSessionRepository(t)
	defer closeDB()
	session := testLabSession()
	query := regexp.QuoteMeta("SELECT " + sessionColumns + " FROM lab_sessions WHERE state IN ($1, $2) ORDER BY updated_at ASC, id ASC LIMIT $3 OFFSET $4")
	mock.ExpectQuery(query).
		WithArgs(SessionStarting, SessionReady, 2, 3).
		WillReturnRows(sqlmock.NewRows(sessionTestColumns).AddRow(sessionRowValues(session)...))
	sessions, err := repository.ListByStates(context.Background(), []SessionState{SessionStarting, SessionReady}, 2, 3)
	if err != nil || len(sessions) != 1 || sessions[0].ID != session.ID {
		t.Fatalf("ListByStates = %#v, %v", sessions, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestLabSessionRepositoryConditionalUpdatesProtectNewerState(t *testing.T) {
	repository, mock, closeDB := newMockLabSessionRepository(t)
	defer closeDB()
	session := testLabSession()
	updatedAt := session.UpdatedAt.Add(time.Minute)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE lab_sessions")).
		WithArgs(session.ID, SessionStarting, SessionReady, updatedAt).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repository.UpdateStateIfCurrent(context.Background(), session.ID, SessionStarting, SessionReady, updatedAt); !errors.Is(err, ErrLabSessionNotFound) {
		t.Fatalf("conditional state error = %v, want ErrLabSessionNotFound", err)
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE lab_sessions")).
		WithArgs(session.ID, SessionReady, "runtime_unavailable", updatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repository.UpdateFailureIfCurrent(context.Background(), session.ID, SessionReady, "runtime_unavailable", updatedAt); err != nil {
		t.Fatalf("conditional failure update returned error: %v", err)
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE lab_sessions")).
		WithArgs(session.ID, SessionStopping, updatedAt, updatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repository.UpdateStoppedIfCurrent(context.Background(), session.ID, SessionStopping, updatedAt, updatedAt); err != nil {
		t.Fatalf("conditional stopped update returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func sessionRowValues(session LabSessionRecord) []driver.Value {
	return []driver.Value{
		session.ID.String(), session.UserID.String(), session.LabID, string(session.State),
		optionalStringValue(session.RuntimeNamespace), optionalStringValue(session.RuntimePodName), optionalStringValue(session.RuntimeServiceName),
		session.CreatedAt, session.UpdatedAt, optionalTimeValue(session.StartedAt), optionalTimeValue(session.StoppedAt), optionalStringValue(session.FailureCode),
	}
}

func optionalStringValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalTimeValue(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}
