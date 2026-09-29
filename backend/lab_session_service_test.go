package main

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakePersistentSessionRepository struct {
	mu                  sync.Mutex
	records             map[uuid.UUID]LabSessionRecord
	createErr           error
	updateStateErr      error
	updateReferencesErr error
	updateFailureErr    error
	updateStoppedErr    error
	createdCount        int
	stateUpdates        []SessionState
	failureCodes        []string
}

func (repository *fakePersistentSessionRepository) Create(_ context.Context, record LabSessionRecord) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.createErr != nil {
		return repository.createErr
	}
	if repository.records == nil {
		repository.records = make(map[uuid.UUID]LabSessionRecord)
	}
	repository.records[record.ID] = record
	repository.createdCount++
	return nil
}

func (repository *fakePersistentSessionRepository) FindByIDAndUserID(_ context.Context, id, userID uuid.UUID) (LabSessionRecord, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	record, ok := repository.records[id]
	if !ok || record.UserID != userID {
		return LabSessionRecord{}, ErrLabSessionNotFound
	}
	return record, nil
}

func (repository *fakePersistentSessionRepository) ListByUserID(_ context.Context, userID uuid.UUID, limit, offset int) ([]LabSessionRecord, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	var records []LabSessionRecord
	for _, record := range repository.records {
		if record.UserID == userID {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(left, right int) bool {
		return records[left].CreatedAt.After(records[right].CreatedAt)
	})
	if limit == 0 {
		limit = defaultSessionPageSize
	}
	if offset >= len(records) {
		return []LabSessionRecord{}, nil
	}
	end := offset + limit
	if end > len(records) {
		end = len(records)
	}
	return records[offset:end], nil
}

func (repository *fakePersistentSessionRepository) UpdateState(_ context.Context, id uuid.UUID, state SessionState, updatedAt time.Time) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.updateStateErr != nil {
		return repository.updateStateErr
	}
	record, ok := repository.records[id]
	if !ok {
		return ErrLabSessionNotFound
	}
	record.State = state
	record.UpdatedAt = updatedAt
	if state == SessionReady && record.StartedAt == nil {
		record.StartedAt = &updatedAt
	}
	repository.records[id] = record
	repository.stateUpdates = append(repository.stateUpdates, state)
	return nil
}

func (repository *fakePersistentSessionRepository) UpdateRuntimeReferences(_ context.Context, id uuid.UUID, namespace, pod, service *string, updatedAt time.Time) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.updateReferencesErr != nil {
		return repository.updateReferencesErr
	}
	record, ok := repository.records[id]
	if !ok {
		return ErrLabSessionNotFound
	}
	record.RuntimeNamespace, record.RuntimePodName, record.RuntimeServiceName = namespace, pod, service
	record.UpdatedAt = updatedAt
	repository.records[id] = record
	return nil
}

func (repository *fakePersistentSessionRepository) UpdateFailure(_ context.Context, id uuid.UUID, code string, updatedAt time.Time) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.updateFailureErr != nil {
		return repository.updateFailureErr
	}
	record, ok := repository.records[id]
	if !ok {
		return ErrLabSessionNotFound
	}
	record.State, record.UpdatedAt = SessionFailed, updatedAt
	record.FailureCode = &code
	repository.records[id] = record
	repository.failureCodes = append(repository.failureCodes, code)
	return nil
}

func (repository *fakePersistentSessionRepository) UpdateStopped(_ context.Context, id uuid.UUID, stoppedAt, updatedAt time.Time) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.updateStoppedErr != nil {
		return repository.updateStoppedErr
	}
	record, ok := repository.records[id]
	if !ok {
		return ErrLabSessionNotFound
	}
	record.State, record.StoppedAt, record.UpdatedAt = SessionStopped, &stoppedAt, updatedAt
	repository.records[id] = record
	return nil
}

type fakeLifecycleRuntime struct {
	mu          sync.Mutex
	createErr   error
	statusErr   error
	stopErr     error
	ready       bool
	createCalls int
	statusCalls int
	stopCalls   int
}

func (runtime *fakeLifecycleRuntime) Create(_ context.Context, request RuntimeCreateRequest) (RuntimeReference, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.createCalls++
	if runtime.createErr != nil {
		return RuntimeReference{}, runtime.createErr
	}
	return RuntimeReference{
		Namespace:   defaultNamespace,
		PodName:     "pod-" + request.SessionID,
		ServiceName: "service-" + request.SessionID,
		Labels:      runtimeLabels(request.SessionID, request.LabID),
	}, nil
}

func (runtime *fakeLifecycleRuntime) Status(_ context.Context, _ RuntimeReference) (RuntimeStatus, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.statusCalls++
	if runtime.statusErr != nil {
		return RuntimeStatus{}, runtime.statusErr
	}
	if runtime.ready {
		return RuntimeStatus{Phase: RuntimeReady, Ready: true}, nil
	}
	return RuntimeStatus{Phase: RuntimeStarting}, nil
}

func (runtime *fakeLifecycleRuntime) Stop(_ context.Context, _ RuntimeReference) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.stopCalls++
	return runtime.stopErr
}

type fakeSessionManager struct {
	runtime  *fakeLifecycleRuntime
	sessions map[string]LabSession
}

func (manager *fakeSessionManager) CreateWithID(ctx context.Context, userID, labID, id string) (LabSession, error) {
	reference, err := manager.runtime.Create(ctx, RuntimeCreateRequest{SessionID: id, LabID: labID})
	if err != nil {
		return LabSession{}, err
	}
	session := LabSession{ID: id, UserID: userID, LabID: labID, State: SessionStarting, runtimeReference: reference}
	if manager.sessions == nil {
		manager.sessions = make(map[string]LabSession)
	}
	manager.sessions[id] = session
	return session, nil
}

func (manager *fakeSessionManager) Get(ctx context.Context, id string) (LabSession, error) {
	session, ok := manager.sessions[id]
	if !ok {
		return LabSession{}, errors.New("session manager record missing")
	}
	status, err := manager.runtime.Status(ctx, session.runtimeReference)
	if err != nil {
		return LabSession{}, err
	}
	if status.Ready {
		session.State = SessionReady
	}
	manager.sessions[id] = session
	return session, nil
}

func (manager *fakeSessionManager) Stop(ctx context.Context, id string) error {
	session, ok := manager.sessions[id]
	if !ok {
		return nil
	}
	if session.State == SessionStopping || session.State == SessionStopped {
		return nil
	}
	if err := manager.runtime.Stop(ctx, session.runtimeReference); err != nil {
		return err
	}
	session.State = SessionStopped
	manager.sessions[id] = session
	return nil
}

func newTestLabSessionService(t *testing.T, repository *fakePersistentSessionRepository, runtime *fakeLifecycleRuntime) *LabSessionService {
	t.Helper()
	manager := &fakeSessionManager{runtime: runtime}
	service, err := NewLabSessionService(repository, manager)
	if err != nil {
		t.Fatalf("NewLabSessionService returned error: %v", err)
	}
	service.newID = func() uuid.UUID {
		return uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	}
	service.now = func() time.Time {
		return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	}
	return service
}

func TestLabSessionServiceCreatesAndReadiesSession(t *testing.T) {
	repository := &fakePersistentSessionRepository{}
	runtime := &fakeLifecycleRuntime{ready: true}
	service := newTestLabSessionService(t, repository, runtime)
	userID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")

	view, err := service.CreateSession(context.Background(), userID, "intro-linux")
	if err != nil {
		t.Fatalf("CreateSession returned error: %v", err)
	}
	if view.State != SessionReady || view.ID == uuid.Nil {
		t.Fatalf("created view = %#v, want READY session", view)
	}
	repository.mu.Lock()
	record := repository.records[view.ID]
	repository.mu.Unlock()
	if record.RuntimePodName == nil || record.StartedAt == nil || len(repository.stateUpdates) != 2 {
		t.Fatalf("persisted record = %#v, state updates = %#v", record, repository.stateUpdates)
	}
	runtime.mu.Lock()
	createCalls, statusCalls := runtime.createCalls, runtime.statusCalls
	runtime.mu.Unlock()
	if createCalls != 1 || statusCalls != 1 {
		t.Fatalf("runtime calls = create:%d status:%d, want one each", createCalls, statusCalls)
	}
}

func TestLabSessionServiceRejectsInvalidInputBeforePersistence(t *testing.T) {
	repository := &fakePersistentSessionRepository{}
	runtime := &fakeLifecycleRuntime{}
	service := newTestLabSessionService(t, repository, runtime)

	for _, input := range []struct {
		userID uuid.UUID
		labID  string
	}{
		{uuid.Nil, "intro-linux"},
		{uuid.New(), "Invalid Lab"},
	} {
		if _, err := service.CreateSession(context.Background(), input.userID, input.labID); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("CreateSession error = %v, want ErrInvalidInput", err)
		}
	}
	if repository.createdCount != 0 || runtime.createCalls != 0 {
		t.Fatalf("invalid input called persistence/runtime: created=%d runtime=%d", repository.createdCount, runtime.createCalls)
	}
}

func TestLabSessionServiceHandlesStartupAndReadinessFailures(t *testing.T) {
	tests := []struct {
		name      string
		runtime   *fakeLifecycleRuntime
		wantCode  string
		wantStops int
	}{
		{name: "runtime startup", runtime: &fakeLifecycleRuntime{createErr: errors.New("runtime details")}, wantCode: failureCodeRuntimeStart, wantStops: 0},
		{name: "not ready", runtime: &fakeLifecycleRuntime{ready: false}, wantCode: failureCodeNotReady, wantStops: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &fakePersistentSessionRepository{}
			service := newTestLabSessionService(t, repository, test.runtime)
			_, err := service.CreateSession(context.Background(), uuid.New(), "intro-linux")
			if err == nil || !errors.Is(err, ErrSessionOperation) && !errors.Is(err, ErrSessionNotReady) {
				t.Fatalf("CreateSession error = %v", err)
			}
			repository.mu.Lock()
			if len(repository.failureCodes) != 1 || repository.failureCodes[0] != test.wantCode {
				t.Fatalf("failure codes = %#v, want %q", repository.failureCodes, test.wantCode)
			}
			repository.mu.Unlock()
			test.runtime.mu.Lock()
			stops := test.runtime.stopCalls
			test.runtime.mu.Unlock()
			if stops != test.wantStops {
				t.Fatalf("stop calls = %d, want %d", stops, test.wantStops)
			}
		})
	}
}

func TestLabSessionServiceHandlesReferencePersistenceFailureWithCleanup(t *testing.T) {
	repository := &fakePersistentSessionRepository{updateReferencesErr: errors.New("database details")}
	runtime := &fakeLifecycleRuntime{ready: true}
	service := newTestLabSessionService(t, repository, runtime)

	_, err := service.CreateSession(context.Background(), uuid.New(), "intro-linux")
	if !errors.Is(err, ErrSessionOperation) {
		t.Fatalf("CreateSession error = %v, want ErrSessionOperation", err)
	}
	runtime.mu.Lock()
	stopCalls := runtime.stopCalls
	runtime.mu.Unlock()
	if stopCalls != 1 {
		t.Fatalf("cleanup stop calls = %d, want 1", stopCalls)
	}
}

func TestLabSessionServiceOwnershipListingAndStopping(t *testing.T) {
	repository := &fakePersistentSessionRepository{}
	runtime := &fakeLifecycleRuntime{ready: true}
	service := newTestLabSessionService(t, repository, runtime)
	ownerID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	otherID := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	view, err := service.CreateSession(context.Background(), ownerID, "intro-linux")
	if err != nil {
		t.Fatalf("CreateSession returned error: %v", err)
	}

	if _, err := service.GetSession(context.Background(), otherID, view.ID); !errors.Is(err, ErrLabSessionNotFound) {
		t.Fatalf("cross-owner GetSession error = %v, want ErrLabSessionNotFound", err)
	}
	listed, err := service.ListUserSessions(context.Background(), ownerID, 10, 0)
	if err != nil || len(listed) != 1 || listed[0].ID != view.ID {
		t.Fatalf("ListUserSessions = %#v, %v", listed, err)
	}
	if _, err := service.StopSession(context.Background(), otherID, view.ID); !errors.Is(err, ErrLabSessionNotFound) {
		t.Fatalf("cross-owner StopSession error = %v, want ErrLabSessionNotFound", err)
	}

	stopped, err := service.StopSession(context.Background(), ownerID, view.ID)
	if err != nil || stopped.State != SessionStopped {
		t.Fatalf("StopSession = %#v, %v", stopped, err)
	}
	if _, err := service.StopSession(context.Background(), ownerID, view.ID); err != nil {
		t.Fatalf("idempotent StopSession returned error: %v", err)
	}
	runtime.mu.Lock()
	stopCalls := runtime.stopCalls
	runtime.mu.Unlock()
	if stopCalls != 1 {
		t.Fatalf("runtime stop calls = %d, want 1", stopCalls)
	}
}

func TestLabSessionServiceHandlesStopFailuresAndFinalPersistenceFailure(t *testing.T) {
	ownerID := uuid.New()
	t.Run("runtime cleanup failure", func(t *testing.T) {
		repository := &fakePersistentSessionRepository{}
		runtime := &fakeLifecycleRuntime{ready: true, stopErr: errors.New("runtime details")}
		service := newTestLabSessionService(t, repository, runtime)
		view, err := service.CreateSession(context.Background(), ownerID, "intro-linux")
		if err != nil {
			t.Fatalf("CreateSession returned error: %v", err)
		}
		_, err = service.StopSession(context.Background(), ownerID, view.ID)
		if !errors.Is(err, ErrSessionOperation) {
			t.Fatalf("StopSession error = %v, want ErrSessionOperation", err)
		}
		repository.mu.Lock()
		failureCount := len(repository.failureCodes)
		repository.mu.Unlock()
		if failureCount != 1 {
			t.Fatalf("failure count = %d, want 1", failureCount)
		}
	})

	t.Run("stopped persistence failure", func(t *testing.T) {
		repository := &fakePersistentSessionRepository{updateStoppedErr: errors.New("database details")}
		runtime := &fakeLifecycleRuntime{ready: true}
		service := newTestLabSessionService(t, repository, runtime)
		view, err := service.CreateSession(context.Background(), ownerID, "intro-linux")
		if err != nil {
			t.Fatalf("CreateSession returned error: %v", err)
		}
		_, err = service.StopSession(context.Background(), ownerID, view.ID)
		if !errors.Is(err, ErrSessionOperation) {
			t.Fatalf("StopSession error = %v, want ErrSessionOperation", err)
		}
		runtime.mu.Lock()
		stopCalls := runtime.stopCalls
		runtime.mu.Unlock()
		if stopCalls != 1 {
			t.Fatalf("runtime stop calls = %d, want 1", stopCalls)
		}
	})
}
