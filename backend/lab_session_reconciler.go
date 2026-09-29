package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	defaultReconciliationTimeout = 10 * time.Second
	reconciliationFailureCode    = "reconciliation_failed"
	runtimeUnavailableCode       = "runtime_unavailable"
	orphanCleanupCode            = "orphan_cleanup"
)

var (
	ErrReconciliationOperation = errors.New("lab session reconciliation failed")
	ErrReconciliationAmbiguous = errors.New("lab session runtime identity is ambiguous")
	ErrReconciliationConflict  = errors.New("lab session changed during reconciliation")
)

type reconciliationRepository interface {
	ListByStates(context.Context, []SessionState, int, int) ([]LabSessionRecord, error)
	FindByID(context.Context, uuid.UUID) (LabSessionRecord, error)
	UpdateStateIfCurrent(context.Context, uuid.UUID, SessionState, SessionState, time.Time) error
	UpdateFailureIfCurrent(context.Context, uuid.UUID, SessionState, string, time.Time) error
	UpdateStoppedIfCurrent(context.Context, uuid.UUID, SessionState, time.Time, time.Time) error
}

type reconciliationRuntime interface {
	Status(context.Context, RuntimeReference) (RuntimeStatus, error)
	Stop(context.Context, RuntimeReference) error
}

type ReconciliationConfig struct {
	OperationTimeout time.Duration
	BatchSize        int
}

type LabSessionReconciler struct {
	repository reconciliationRepository
	runtime    reconciliationRuntime
	config     ReconciliationConfig
	now        func() time.Time
	locks      sync.Map
}

type ReconciliationResult struct {
	SessionID    uuid.UUID `json:"session_id"`
	Observed     string    `json:"observed"`
	Action       string    `json:"action"`
	FailureCode  string    `json:"failure_code,omitempty"`
	StateChanged bool      `json:"state_changed"`
}

func NewLabSessionReconciler(repository reconciliationRepository, runtime reconciliationRuntime, config ReconciliationConfig) (*LabSessionReconciler, error) {
	if repository == nil || runtime == nil {
		return nil, errors.New("reconciliation dependencies are required")
	}
	if config.OperationTimeout <= 0 {
		config.OperationTimeout = defaultReconciliationTimeout
	}
	if config.BatchSize <= 0 || config.BatchSize > maxSessionPageSize {
		config.BatchSize = defaultSessionPageSize
	}
	return &LabSessionReconciler{
		repository: repository,
		runtime:    runtime,
		config:     config,
		now:        time.Now,
	}, nil
}

func (reconciler *LabSessionReconciler) ReconcileBatch(ctx context.Context, limit, offset int) ([]ReconciliationResult, error) {
	if limit < 0 || offset < 0 || limit > maxSessionPageSize {
		return nil, ErrInvalidInput
	}
	if limit == 0 {
		limit = reconciler.config.BatchSize
	}
	listContext, cancel := context.WithTimeout(ctx, reconciler.config.OperationTimeout)
	defer cancel()
	states := []SessionState{SessionRequested, SessionStarting, SessionReady, SessionStopping, SessionStopped, SessionFailed}
	sessions, err := reconciler.repository.ListByStates(listContext, states, limit, offset)
	if err != nil {
		return nil, err
	}
	results := make([]ReconciliationResult, 0, len(sessions))
	for _, session := range sessions {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		result, reconcileErr := reconciler.ReconcileSession(ctx, session.ID)
		results = append(results, result)
		if reconcileErr != nil {
			return results, reconcileErr
		}
	}
	return results, nil
}

func (reconciler *LabSessionReconciler) ReconcileSession(ctx context.Context, sessionID uuid.UUID) (ReconciliationResult, error) {
	result := ReconciliationResult{SessionID: sessionID}
	if sessionID == uuid.Nil {
		return result, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	lock := reconciler.sessionLock(sessionID)
	lock.Lock()
	defer lock.Unlock()

	operationContext, cancel := context.WithTimeout(ctx, reconciler.config.OperationTimeout)
	defer cancel()
	session, err := reconciler.repository.FindByID(operationContext, sessionID)
	if err != nil {
		return result, err
	}
	result.Observed = string(session.State)

	switch session.State {
	case SessionRequested:
		result.Action = "no_action"
		return result, nil
	case SessionStarting, SessionReady, SessionStopping, SessionStopped, SessionFailed:
		return reconciler.reconcileRuntimeState(operationContext, session, result)
	default:
		return result, ErrInvalidInput
	}
}

func (reconciler *LabSessionReconciler) reconcileRuntimeState(ctx context.Context, session LabSessionRecord, result ReconciliationResult) (ReconciliationResult, error) {
	reference, err := runtimeReferenceForRecord(session)
	if err != nil {
		result.Action = "ambiguous_identity"
		result.FailureCode = orphanCleanupCode
		if session.State == SessionStarting || session.State == SessionReady {
			if updateErr := reconciler.repository.UpdateFailureIfCurrent(ctx, session.ID, session.State, orphanCleanupCode, reconciler.now().UTC()); updateErr != nil && !errors.Is(updateErr, ErrLabSessionNotFound) {
				return result, updateErr
			}
			result.StateChanged = true
		}
		return result, ErrReconciliationAmbiguous
	}

	status, err := reconciler.runtime.Status(ctx, reference)
	if err != nil {
		result.Action = "runtime_lookup_failed"
		result.FailureCode = reconciliationFailureCode
		return result, fmt.Errorf("%w: runtime lookup", ErrReconciliationOperation)
	}

	switch session.State {
	case SessionStarting:
		return reconciler.reconcileStarting(ctx, session, reference, status, result)
	case SessionReady:
		return reconciler.reconcileReady(ctx, session, status, result)
	case SessionStopping:
		return reconciler.reconcileStopping(ctx, session, reference, status, result)
	case SessionStopped:
		return reconciler.reconcileOrphaned(ctx, session, reference, status, result)
	case SessionFailed:
		return reconciler.reconcileFailed(ctx, session, reference, status, result)
	default:
		return result, ErrInvalidInput
	}
}

func (reconciler *LabSessionReconciler) reconcileStarting(ctx context.Context, session LabSessionRecord, reference RuntimeReference, status RuntimeStatus, result ReconciliationResult) (ReconciliationResult, error) {
	switch status.Phase {
	case RuntimeReady:
		if !status.Ready {
			result.Action = "still_progressing"
			return result, nil
		}
		if err := reconciler.repository.UpdateStateIfCurrent(ctx, session.ID, SessionStarting, SessionReady, reconciler.now().UTC()); err != nil {
			return reconciler.conditionalUpdateResult(result, err)
		}
		result.Action = "marked_ready"
		result.StateChanged = true
		return result, nil
	case RuntimeStarting:
		result.Action = "still_progressing"
		return result, nil
	case RuntimeMissing, RuntimeOrphaned, RuntimeStopped, RuntimeFailed:
		return reconciler.markFailed(ctx, session, runtimeUnavailableCode, result)
	default:
		return result, ErrReconciliationOperation
	}
}

func (reconciler *LabSessionReconciler) reconcileReady(ctx context.Context, session LabSessionRecord, status RuntimeStatus, result ReconciliationResult) (ReconciliationResult, error) {
	if status.Phase == RuntimeReady && status.Ready {
		result.Action = "no_action"
		return result, nil
	}
	return reconciler.markFailed(ctx, session, runtimeUnavailableCode, result)
}

func (reconciler *LabSessionReconciler) reconcileStopping(ctx context.Context, session LabSessionRecord, reference RuntimeReference, status RuntimeStatus, result ReconciliationResult) (ReconciliationResult, error) {
	if status.Phase == RuntimeMissing || status.Phase == RuntimeOrphaned {
		return reconciler.markStopped(ctx, session, result)
	}
	return reconciler.cleanupAndConfirm(ctx, session, reference, result)
}

func (reconciler *LabSessionReconciler) reconcileOrphaned(ctx context.Context, session LabSessionRecord, reference RuntimeReference, status RuntimeStatus, result ReconciliationResult) (ReconciliationResult, error) {
	if status.Phase == RuntimeMissing || status.Phase == RuntimeOrphaned {
		result.Action = "no_action"
		return result, nil
	}
	return reconciler.cleanupAndConfirm(ctx, session, reference, result)
}

func (reconciler *LabSessionReconciler) reconcileFailed(ctx context.Context, session LabSessionRecord, reference RuntimeReference, status RuntimeStatus, result ReconciliationResult) (ReconciliationResult, error) {
	if status.Phase == RuntimeMissing || status.Phase == RuntimeOrphaned {
		result.Action = "no_action"
		return result, nil
	}
	result.FailureCode = session.FailureCodeValue()
	return reconciler.cleanupAndConfirm(ctx, session, reference, result)
}

func (reconciler *LabSessionReconciler) cleanupAndConfirm(ctx context.Context, session LabSessionRecord, reference RuntimeReference, result ReconciliationResult) (ReconciliationResult, error) {
	if err := reconciler.runtime.Stop(ctx, reference); err != nil {
		result.Action = "cleanup_failed"
		return result, fmt.Errorf("%w: runtime cleanup", ErrReconciliationOperation)
	}
	status, err := reconciler.runtime.Status(ctx, reference)
	if err != nil {
		result.Action = "cleanup_unconfirmed"
		return result, fmt.Errorf("%w: cleanup verification", ErrReconciliationOperation)
	}
	if status.Phase != RuntimeMissing {
		result.Action = "cleanup_pending"
		return result, nil
	}
	if session.State == SessionStopping {
		return reconciler.markStopped(ctx, session, result)
	}
	result.Action = "orphan_cleaned"
	return result, nil
}

func (reconciler *LabSessionReconciler) markFailed(ctx context.Context, session LabSessionRecord, failureCode string, result ReconciliationResult) (ReconciliationResult, error) {
	if err := reconciler.repository.UpdateFailureIfCurrent(ctx, session.ID, session.State, failureCode, reconciler.now().UTC()); err != nil {
		return reconciler.conditionalUpdateResult(result, err)
	}
	result.Action = "marked_failed"
	result.FailureCode = failureCode
	result.StateChanged = true
	return result, nil
}

func (reconciler *LabSessionReconciler) markStopped(ctx context.Context, session LabSessionRecord, result ReconciliationResult) (ReconciliationResult, error) {
	now := reconciler.now().UTC()
	if err := reconciler.repository.UpdateStoppedIfCurrent(ctx, session.ID, session.State, now, now); err != nil {
		return reconciler.conditionalUpdateResult(result, err)
	}
	result.Action = "marked_stopped"
	result.StateChanged = true
	return result, nil
}

func (reconciler *LabSessionReconciler) conditionalUpdateResult(result ReconciliationResult, err error) (ReconciliationResult, error) {
	if errors.Is(err, ErrLabSessionNotFound) {
		result.Action = "stale_record"
		return result, ErrReconciliationConflict
	}
	result.Action = "database_update_failed"
	return result, fmt.Errorf("%w: database update", ErrReconciliationOperation)
}

func (reconciler *LabSessionReconciler) sessionLock(id uuid.UUID) *sync.Mutex {
	value, _ := reconciler.locks.LoadOrStore(id, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func runtimeReferenceForRecord(session LabSessionRecord) (RuntimeReference, error) {
	if session.ID == uuid.Nil || session.RuntimeNamespace == nil || session.RuntimePodName == nil || session.RuntimeServiceName == nil {
		return RuntimeReference{}, ErrReconciliationAmbiguous
	}
	if *session.RuntimeNamespace == "" || *session.RuntimePodName == "" || *session.RuntimeServiceName == "" {
		return RuntimeReference{}, ErrReconciliationAmbiguous
	}
	sessionID := session.ID.String()
	expectedNames := runtimeNames(sessionID)
	if *session.RuntimePodName != expectedNames.pod || *session.RuntimeServiceName != expectedNames.service {
		return RuntimeReference{}, ErrReconciliationAmbiguous
	}
	return RuntimeReference{
		Namespace:   *session.RuntimeNamespace,
		PodName:     *session.RuntimePodName,
		ServiceName: *session.RuntimeServiceName,
		Labels:      runtimeLabels(sessionID, session.LabID),
	}, nil
}

func (session LabSessionRecord) FailureCodeValue() string {
	if session.FailureCode == nil {
		return ""
	}
	return *session.FailureCode
}
