package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeReconciliationRepository struct {
	mu             sync.Mutex
	records        map[uuid.UUID]LabSessionRecord
	listCalls      int
	listLimit      int
	updateErr      error
	failureErr     error
	stopUpdateErr  error
	stateUpdates   int
	failureUpdates int
	stoppedUpdates int
}

func (repository *fakeReconciliationRepository) ListByStates(_ context.Context, states []SessionState, limit, offset int) ([]LabSessionRecord, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	repository.listCalls++
	repository.listLimit = limit
	result := make([]LabSessionRecord, 0, limit)
	for _, record := range repository.records {
		for _, state := range states {
			if record.State == state {
				result = append(result, record)
				break
			}
		}
	}
	if offset >= len(result) {
		return []LabSessionRecord{}, nil
	}
	end := offset + limit
	if end > len(result) {
		end = len(result)
	}
	return result[offset:end], nil
}

func (repository *fakeReconciliationRepository) FindByID(_ context.Context, id uuid.UUID) (LabSessionRecord, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	record, ok := repository.records[id]
	if !ok {
		return LabSessionRecord{}, ErrLabSessionNotFound
	}
	return record, nil
}

func (repository *fakeReconciliationRepository) UpdateStateIfCurrent(_ context.Context, id uuid.UUID, expected, next SessionState, updatedAt time.Time) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.updateErr != nil {
		return repository.updateErr
	}
	record, ok := repository.records[id]
	if !ok {
		return ErrLabSessionNotFound
	}
	if record.State != expected {
		return ErrLabSessionNotFound
	}
	record.State, record.UpdatedAt = next, updatedAt
	repository.records[id] = record
	repository.stateUpdates++
	return nil
}

func (repository *fakeReconciliationRepository) UpdateFailureIfCurrent(_ context.Context, id uuid.UUID, expected SessionState, code string, updatedAt time.Time) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.failureErr != nil {
		return repository.failureErr
	}
	record, ok := repository.records[id]
	if !ok {
		return ErrLabSessionNotFound
	}
	if record.State != expected {
		return ErrLabSessionNotFound
	}
	record.State, record.UpdatedAt = SessionFailed, updatedAt
	record.FailureCode = &code
	repository.records[id] = record
	repository.failureUpdates++
	return nil
}

func (repository *fakeReconciliationRepository) UpdateStoppedIfCurrent(_ context.Context, id uuid.UUID, expected SessionState, stoppedAt, updatedAt time.Time) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.stopUpdateErr != nil {
		return repository.stopUpdateErr
	}
	record, ok := repository.records[id]
	if !ok {
		return ErrLabSessionNotFound
	}
	if record.State != expected {
		return ErrLabSessionNotFound
	}
	record.State, record.StoppedAt, record.UpdatedAt = SessionStopped, &stoppedAt, updatedAt
	repository.records[id] = record
	repository.stoppedUpdates++
	return nil
}

type fakeReconciliationRuntime struct {
	mu              sync.Mutex
	status          RuntimeStatus
	statusErr       error
	stopErr         error
	stopSetsMissing bool
	statusCalls     int
	stopCalls       int
	statusStarted   chan struct{}
	continueStatus  chan struct{}
}

func (runtime *fakeReconciliationRuntime) Status(ctx context.Context, _ RuntimeReference) (RuntimeStatus, error) {
	if runtime.statusStarted != nil {
		select {
		case runtime.statusStarted <- struct{}{}:
		default:
		}
	}
	if runtime.continueStatus != nil {
		select {
		case <-runtime.continueStatus:
		case <-ctx.Done():
			return RuntimeStatus{}, ctx.Err()
		}
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.statusCalls++
	return runtime.status, runtime.statusErr
}

func (runtime *fakeReconciliationRuntime) Stop(_ context.Context, _ RuntimeReference) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.stopCalls++
	if runtime.stopSetsMissing {
		runtime.status = RuntimeStatus{Phase: RuntimeMissing}
	}
	return runtime.stopErr
}

func newTestReconciler(t *testing.T, record LabSessionRecord, runtime *fakeReconciliationRuntime) (*LabSessionReconciler, *fakeReconciliationRepository) {
	t.Helper()
	repository := &fakeReconciliationRepository{records: map[uuid.UUID]LabSessionRecord{record.ID: record}}
	reconciler, err := NewLabSessionReconciler(repository, runtime, ReconciliationConfig{OperationTimeout: time.Second, BatchSize: 2})
	if err != nil {
		t.Fatalf("NewLabSessionReconciler returned error: %v", err)
	}
	reconciler.now = func() time.Time { return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC) }
	return reconciler, repository
}

func reconciliationSession(state SessionState) LabSessionRecord {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	session := LabSessionRecord{
		ID:        uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
		UserID:    uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
		LabID:     "intro-linux",
		State:     state,
		CreatedAt: now,
		UpdatedAt: now,
	}
	namespace, pod, service := defaultNamespace, runtimeNames(session.ID.String()).pod, runtimeNames(session.ID.String()).service
	session.RuntimeNamespace, session.RuntimePodName, session.RuntimeServiceName = &namespace, &pod, &service
	return session
}

func TestReconcilerStartingReadyAndProgressing(t *testing.T) {
	runtime := &fakeReconciliationRuntime{status: RuntimeStatus{Phase: RuntimeReady, Ready: true}}
	reconciler, repository := newTestReconciler(t, reconciliationSession(SessionStarting), runtime)
	result, err := reconciler.ReconcileSession(context.Background(), reconciliationSession(SessionStarting).ID)
	if err != nil || result.Action != "marked_ready" {
		t.Fatalf("ready reconciliation = %#v, %v", result, err)
	}
	if repository.records[result.SessionID].State != SessionReady {
		t.Fatalf("state = %s, want READY", repository.records[result.SessionID].State)
	}

	repository.records[result.SessionID] = reconciliationSession(SessionStarting)
	runtime.status = RuntimeStatus{Phase: RuntimeStarting}
	result, err = reconciler.ReconcileSession(context.Background(), result.SessionID)
	if err != nil || result.Action != "still_progressing" {
		t.Fatalf("progressing reconciliation = %#v, %v", result, err)
	}
}

func TestReconcilerMissingAndUnavailableResourcesBecomeFailed(t *testing.T) {
	for _, state := range []SessionState{SessionStarting, SessionReady} {
		t.Run(string(state), func(t *testing.T) {
			runtime := &fakeReconciliationRuntime{status: RuntimeStatus{Phase: RuntimeMissing}}
			reconciler, repository := newTestReconciler(t, reconciliationSession(state), runtime)
			result, err := reconciler.ReconcileSession(context.Background(), reconciliationSession(state).ID)
			if err != nil || result.Action != "marked_failed" || repository.records[result.SessionID].State != SessionFailed {
				t.Fatalf("reconciliation = %#v, %v, record=%#v", result, err, repository.records[result.SessionID])
			}
		})
	}
}

func TestReconcilerStoppingCleansAndMarksStopped(t *testing.T) {
	runtime := &fakeReconciliationRuntime{status: RuntimeStatus{Phase: RuntimeStarting}, stopSetsMissing: true}
	reconciler, repository := newTestReconciler(t, reconciliationSession(SessionStopping), runtime)
	result, err := reconciler.ReconcileSession(context.Background(), reconciliationSession(SessionStopping).ID)
	if err != nil || result.Action != "marked_stopped" || repository.records[result.SessionID].State != SessionStopped {
		t.Fatalf("stopping reconciliation = %#v, %v", result, err)
	}
	if runtime.stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", runtime.stopCalls)
	}
}

func TestReconcilerStoppingMissingIsIdempotentAndStoppedOrphanCleans(t *testing.T) {
	missingRuntime := &fakeReconciliationRuntime{status: RuntimeStatus{Phase: RuntimeMissing}}
	reconciler, repository := newTestReconciler(t, reconciliationSession(SessionStopping), missingRuntime)
	result, err := reconciler.ReconcileSession(context.Background(), reconciliationSession(SessionStopping).ID)
	if err != nil || result.Action != "marked_stopped" {
		t.Fatalf("missing stopping reconciliation = %#v, %v", result, err)
	}

	stopped := reconciliationSession(SessionStopped)
	orphanRuntime := &fakeReconciliationRuntime{status: RuntimeStatus{Phase: RuntimeReady, Ready: true}, stopSetsMissing: true}
	reconciler, repository = newTestReconciler(t, stopped, orphanRuntime)
	result, err = reconciler.ReconcileSession(context.Background(), stopped.ID)
	if err != nil || result.Action != "orphan_cleaned" || repository.records[stopped.ID].State != SessionStopped {
		t.Fatalf("orphan reconciliation = %#v, %v", result, err)
	}
}

func TestReconcilerFailedPreservesFailureWhileCleaning(t *testing.T) {
	failure := "runtime_start_failed"
	session := reconciliationSession(SessionFailed)
	session.FailureCode = &failure
	runtime := &fakeReconciliationRuntime{status: RuntimeStatus{Phase: RuntimeReady, Ready: true}, stopSetsMissing: true}
	reconciler, repository := newTestReconciler(t, session, runtime)
	result, err := reconciler.ReconcileSession(context.Background(), session.ID)
	if err != nil || result.Action != "orphan_cleaned" || repository.records[session.ID].State != SessionFailed || result.FailureCode != failure {
		t.Fatalf("failed reconciliation = %#v, %v", result, err)
	}
}

func TestReconcilerAmbiguousIdentityNeverCleans(t *testing.T) {
	session := reconciliationSession(SessionStopped)
	session.RuntimePodName = stringPointer("unrelated-pod")
	runtime := &fakeReconciliationRuntime{status: RuntimeStatus{Phase: RuntimeReady, Ready: true}}
	reconciler, _ := newTestReconciler(t, session, runtime)
	result, err := reconciler.ReconcileSession(context.Background(), session.ID)
	if !errors.Is(err, ErrReconciliationAmbiguous) || result.Action != "ambiguous_identity" || runtime.stopCalls != 0 {
		t.Fatalf("ambiguous reconciliation = %#v, %v, stops=%d", result, err, runtime.stopCalls)
	}
}

func TestReconcilerRuntimeAndDatabaseErrorsAreSafe(t *testing.T) {
	session := reconciliationSession(SessionReady)
	runtime := &fakeReconciliationRuntime{statusErr: errors.New("private runtime details")}
	reconciler, _ := newTestReconciler(t, session, runtime)
	result, err := reconciler.ReconcileSession(context.Background(), session.ID)
	if !errors.Is(err, ErrReconciliationOperation) || result.Action != "runtime_lookup_failed" || strings.Contains(err.Error(), "private runtime") {
		t.Fatalf("runtime error = %#v, %v", result, err)
	}

	runtime.statusErr = nil
	runtime.status = RuntimeStatus{Phase: RuntimeStarting}
	repository := &fakeReconciliationRepository{records: map[uuid.UUID]LabSessionRecord{session.ID: session}, failureErr: errors.New("private database details")}
	reconciler, err = NewLabSessionReconciler(repository, runtime, ReconciliationConfig{})
	if err != nil {
		t.Fatalf("NewLabSessionReconciler returned error: %v", err)
	}
	_, err = reconciler.ReconcileSession(context.Background(), session.ID)
	if !errors.Is(err, ErrReconciliationOperation) || strings.Contains(err.Error(), "private database") {
		t.Fatalf("database error = %v", err)
	}
}

func TestReconcilerSerializesConcurrentAttemptsAndHonorsCancellation(t *testing.T) {
	session := reconciliationSession(SessionStarting)
	runtime := &fakeReconciliationRuntime{
		status:         RuntimeStatus{Phase: RuntimeStarting},
		statusStarted:  make(chan struct{}, 2),
		continueStatus: make(chan struct{}),
	}
	reconciler, _ := newTestReconciler(t, session, runtime)
	firstDone := make(chan struct{})
	go func() {
		_, _ = reconciler.ReconcileSession(context.Background(), session.ID)
		close(firstDone)
	}()
	<-runtime.statusStarted
	secondDone := make(chan struct{})
	go func() {
		_, _ = reconciler.ReconcileSession(context.Background(), session.ID)
		close(secondDone)
	}()
	select {
	case <-secondDone:
		t.Fatal("second reconciliation completed while first was holding the session lock")
	case <-time.After(20 * time.Millisecond):
	}
	close(runtime.continueStatus)
	<-firstDone
	<-secondDone

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := reconciler.ReconcileSession(cancelled, session.ID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reconciliation error = %v, want context.Canceled", err)
	}
}

func TestReconcilerHonorsOperationTimeout(t *testing.T) {
	session := reconciliationSession(SessionStarting)
	runtime := &fakeReconciliationRuntime{
		status:         RuntimeStatus{Phase: RuntimeStarting},
		continueStatus: make(chan struct{}),
	}
	repository := &fakeReconciliationRepository{records: map[uuid.UUID]LabSessionRecord{session.ID: session}}
	reconciler, err := NewLabSessionReconciler(repository, runtime, ReconciliationConfig{OperationTimeout: time.Millisecond})
	if err != nil {
		t.Fatalf("NewLabSessionReconciler returned error: %v", err)
	}
	_, err = reconciler.ReconcileSession(context.Background(), session.ID)
	if !errors.Is(err, ErrReconciliationOperation) {
		t.Fatalf("timeout error = %v, want ErrReconciliationOperation", err)
	}
}

func TestReconcilerBatchIsBounded(t *testing.T) {
	session := reconciliationSession(SessionRequested)
	repository := &fakeReconciliationRepository{records: map[uuid.UUID]LabSessionRecord{session.ID: session}}
	runtime := &fakeReconciliationRuntime{}
	reconciler, err := NewLabSessionReconciler(repository, runtime, ReconciliationConfig{BatchSize: 2})
	if err != nil {
		t.Fatalf("NewLabSessionReconciler returned error: %v", err)
	}
	results, err := reconciler.ReconcileBatch(context.Background(), 0, 0)
	if err != nil || len(results) != 1 || repository.listLimit != 2 {
		t.Fatalf("batch = %#v, %v, list limit=%d", results, err, repository.listLimit)
	}
}
