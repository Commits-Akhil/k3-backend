package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const (
	failureCodeRuntimeStart     = "runtime_start_failed"
	failureCodeReferencePersist = "runtime_reference_persist_failed"
	failureCodeNotReady         = "runtime_not_ready"
	failureCodeRuntimeStatus    = "runtime_status_failed"
	failureCodeRuntimeStop      = "runtime_stop_failed"
	failureCodeReadyPersist     = "ready_state_persist_failed"
	failureCodeStoppedPersist   = "stopped_state_persist_failed"
)

var (
	ErrSessionNotReady  = errors.New("lab session is not ready")
	ErrSessionOperation = errors.New("lab session operation failed")
	ErrSessionCleanup   = errors.New("lab session cleanup failed")
)

type persistentLabSessionRepository interface {
	Create(context.Context, LabSessionRecord) error
	FindByIDAndUserID(context.Context, uuid.UUID, uuid.UUID) (LabSessionRecord, error)
	ListByUserID(context.Context, uuid.UUID, int, int) ([]LabSessionRecord, error)
	UpdateState(context.Context, uuid.UUID, SessionState, time.Time) error
	UpdateRuntimeReferences(context.Context, uuid.UUID, *string, *string, *string, time.Time) error
	UpdateFailure(context.Context, uuid.UUID, string, time.Time) error
	UpdateStopped(context.Context, uuid.UUID, time.Time, time.Time) error
}

type sessionManagerCoordinator interface {
	CreateWithID(context.Context, string, string, string) (LabSession, error)
	Get(context.Context, string) (LabSession, error)
	Stop(context.Context, string) error
}

type LabSessionService struct {
	repository persistentLabSessionRepository
	manager    sessionManagerCoordinator
	now        func() time.Time
	newID      func() uuid.UUID
}

type LabSessionView struct {
	ID        uuid.UUID    `json:"id"`
	LabID     string       `json:"lab_id"`
	State     SessionState `json:"state"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
	StartedAt *time.Time   `json:"started_at,omitempty"`
	StoppedAt *time.Time   `json:"stopped_at,omitempty"`
}

func NewLabSessionService(repository persistentLabSessionRepository, manager sessionManagerCoordinator) (*LabSessionService, error) {
	if repository == nil || manager == nil {
		return nil, errors.New("lab session service dependencies are required")
	}
	return &LabSessionService{
		repository: repository,
		manager:    manager,
		now:        time.Now,
		newID:      uuid.New,
	}, nil
}

func (service *LabSessionService) CreateSession(ctx context.Context, userID uuid.UUID, labID string) (LabSessionView, error) {
	if userID == uuid.Nil {
		return LabSessionView{}, ErrInvalidInput
	}
	if err := validateLabIdentifier(labID); err != nil {
		return LabSessionView{}, ErrInvalidInput
	}

	sessionID := service.newID()
	now := service.now().UTC()
	record := LabSessionRecord{
		ID:        sessionID,
		UserID:    userID,
		LabID:     labID,
		State:     SessionRequested,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := service.repository.Create(ctx, record); err != nil {
		return LabSessionView{}, err
	}
	if err := service.repository.UpdateState(ctx, sessionID, SessionStarting, now); err != nil {
		return service.failCreation(ctx, sessionID, failureCodeRuntimeStart, err)
	}

	managedSession, err := service.manager.CreateWithID(ctx, userID.String(), labID, sessionID.String())
	if err != nil {
		return service.failCreation(ctx, sessionID, failureCodeRuntimeStart, err)
	}
	if managedSession.ID != sessionID.String() || managedSession.runtimeReference.Namespace == "" || managedSession.runtimeReference.PodName == "" || managedSession.runtimeReference.ServiceName == "" {
		cleanupErr := service.manager.Stop(ctx, sessionID.String())
		failureErr := service.recordFailure(ctx, sessionID, failureCodeRuntimeStart)
		return LabSessionView{}, combineLifecycleErrors(ErrSessionOperation, cleanupErr, failureErr)
	}
	if err := service.repository.UpdateRuntimeReferences(
		ctx,
		sessionID,
		stringPointer(managedSession.runtimeReference.Namespace),
		stringPointer(managedSession.runtimeReference.PodName),
		stringPointer(managedSession.runtimeReference.ServiceName),
		now,
	); err != nil {
		cleanupErr := service.manager.Stop(ctx, sessionID.String())
		failureErr := service.recordFailure(ctx, sessionID, failureCodeReferencePersist)
		return LabSessionView{}, combineLifecycleErrors(ErrSessionOperation, err, cleanupErr, failureErr)
	}

	readySession, err := service.manager.Get(ctx, sessionID.String())
	if err != nil {
		cleanupErr := service.manager.Stop(ctx, sessionID.String())
		failureErr := service.recordFailure(ctx, sessionID, failureCodeRuntimeStatus)
		return LabSessionView{}, combineLifecycleErrors(ErrSessionOperation, err, cleanupErr, failureErr)
	}
	if readySession.State != SessionReady {
		cleanupErr := service.manager.Stop(ctx, sessionID.String())
		failureErr := service.recordFailure(ctx, sessionID, failureCodeNotReady)
		return LabSessionView{}, combineLifecycleErrors(ErrSessionNotReady, nil, cleanupErr, failureErr)
	}
	if err := service.repository.UpdateState(ctx, sessionID, SessionReady, service.now().UTC()); err != nil {
		cleanupErr := service.manager.Stop(ctx, sessionID.String())
		failureErr := service.recordFailure(ctx, sessionID, failureCodeReadyPersist)
		return LabSessionView{}, combineLifecycleErrors(ErrSessionOperation, cleanupErr, failureErr)
	}

	return service.GetSession(ctx, userID, sessionID)
}

func (service *LabSessionService) GetSession(ctx context.Context, userID, sessionID uuid.UUID) (LabSessionView, error) {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return LabSessionView{}, ErrInvalidInput
	}
	record, err := service.repository.FindByIDAndUserID(ctx, sessionID, userID)
	if err != nil {
		return LabSessionView{}, err
	}
	return labSessionView(record), nil
}

func (service *LabSessionService) GetTerminalReference(ctx context.Context, userID, sessionID uuid.UUID) (RuntimeReference, error) {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return RuntimeReference{}, ErrInvalidInput
	}
	record, err := service.repository.FindByIDAndUserID(ctx, sessionID, userID)
	if err != nil {
		return RuntimeReference{}, err
	}
	if record.State != SessionReady {
		return RuntimeReference{}, ErrSessionNotReady
	}
	reference, err := runtimeReferenceForRecord(record)
	if err != nil {
		return RuntimeReference{}, ErrSessionOperation
	}
	return reference, nil
}

func (service *LabSessionService) ListUserSessions(ctx context.Context, userID uuid.UUID, limit, offset int) ([]LabSessionView, error) {
	if userID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	records, err := service.repository.ListByUserID(ctx, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	views := make([]LabSessionView, 0, len(records))
	for _, record := range records {
		views = append(views, labSessionView(record))
	}
	return views, nil
}

func (service *LabSessionService) StopSession(ctx context.Context, userID, sessionID uuid.UUID) (LabSessionView, error) {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return LabSessionView{}, ErrInvalidInput
	}
	record, err := service.repository.FindByIDAndUserID(ctx, sessionID, userID)
	if err != nil {
		return LabSessionView{}, err
	}
	if record.State == SessionStopped {
		return labSessionView(record), nil
	}
	if record.State != SessionStopping {
		if err := service.repository.UpdateState(ctx, sessionID, SessionStopping, service.now().UTC()); err != nil {
			return LabSessionView{}, err
		}
	}

	if err := service.manager.Stop(ctx, sessionID.String()); err != nil {
		failureErr := service.recordFailure(ctx, sessionID, failureCodeRuntimeStop)
		return LabSessionView{}, combineLifecycleErrors(ErrSessionOperation, err, failureErr)
	}
	stoppedAt := service.now().UTC()
	if err := service.repository.UpdateStopped(ctx, sessionID, stoppedAt, stoppedAt); err != nil {
		failureErr := service.recordFailure(ctx, sessionID, failureCodeStoppedPersist)
		return LabSessionView{}, combineLifecycleErrors(ErrSessionOperation, failureErr)
	}
	return service.GetSession(ctx, userID, sessionID)
}

func (service *LabSessionService) failCreation(ctx context.Context, sessionID uuid.UUID, failureCode string, original error) (LabSessionView, error) {
	failureErr := service.recordFailure(ctx, sessionID, failureCode)
	return LabSessionView{}, combineLifecycleErrors(ErrSessionOperation, original, failureErr)
}

func (service *LabSessionService) recordFailure(ctx context.Context, sessionID uuid.UUID, failureCode string) error {
	return service.repository.UpdateFailure(ctx, sessionID, failureCode, service.now().UTC())
}

func labSessionView(record LabSessionRecord) LabSessionView {
	return LabSessionView{
		ID:        record.ID,
		LabID:     record.LabID,
		State:     record.State,
		CreatedAt: record.CreatedAt,
		UpdatedAt: record.UpdatedAt,
		StartedAt: record.StartedAt,
		StoppedAt: record.StoppedAt,
	}
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func combineLifecycleErrors(base error, errorsToCombine ...error) error {
	combined := base
	for _, err := range errorsToCombine {
		if err != nil {
			combined = fmt.Errorf("%w: %w", combined, ErrSessionCleanup)
		}
	}
	return combined
}
