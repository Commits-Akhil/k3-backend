package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"
)

var labIdentifierPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

type LabSession struct {
	ID        string
	UserID    string
	LabID     string
	State     SessionState
	CreatedAt time.Time
	UpdatedAt time.Time

	runtimeReference RuntimeReference
}

type SessionManager struct {
	mu       sync.RWMutex
	runtime  LabRuntime
	sessions map[string]*LabSession
	now      func() time.Time
	newID    func() (string, error)
}

func NewSessionManager(runtime LabRuntime) (*SessionManager, error) {
	if runtime == nil {
		return nil, errors.New("lab runtime is required")
	}

	return &SessionManager{
		runtime:  runtime,
		sessions: make(map[string]*LabSession),
		now:      time.Now,
		newID:    newSessionID,
	}, nil
}

func (manager *SessionManager) Create(ctx context.Context, userID, labID string) (LabSession, error) {
	id, err := manager.newID()
	if err != nil {
		return LabSession{}, fmt.Errorf("generate session ID: %w", err)
	}
	return manager.CreateWithID(ctx, userID, labID, id)
}

// CreateWithID lets a persistent coordinator use the same opaque ID in memory
// and in the runtime while preserving the existing Create behavior.
func (manager *SessionManager) CreateWithID(ctx context.Context, userID, labID, id string) (LabSession, error) {
	if err := validateLabIdentifier(labID); err != nil {
		return LabSession{}, err
	}
	if err := validateSessionID(id); err != nil {
		return LabSession{}, err
	}

	now := manager.now()
	session := &LabSession{
		ID:        id,
		UserID:    userID,
		LabID:     labID,
		State:     SessionRequested,
		CreatedAt: now,
		UpdatedAt: now,
	}

	manager.mu.Lock()
	manager.sessions[id] = session
	manager.mu.Unlock()

	reference, err := manager.runtime.Create(ctx, RuntimeCreateRequest{
		SessionID: id,
		LabID:     labID,
	})
	if err != nil {
		manager.setState(id, SessionFailed)
		return LabSession{}, fmt.Errorf("create lab runtime: %w", err)
	}

	manager.mu.Lock()
	session.runtimeReference = reference
	session.State = SessionStarting
	session.UpdatedAt = manager.now()
	result := copySession(session)
	manager.mu.Unlock()

	return result, nil
}

func (manager *SessionManager) Get(ctx context.Context, sessionID string) (LabSession, error) {
	manager.mu.RLock()
	session, ok := manager.sessions[sessionID]
	if !ok {
		manager.mu.RUnlock()
		return LabSession{}, errors.New("session not found")
	}
	current := copySession(session)
	manager.mu.RUnlock()

	if current.State == SessionStopped || current.State == SessionFailed {
		return current, nil
	}

	status, err := manager.runtime.Status(ctx, current.runtimeReference)
	if err != nil {
		manager.setState(sessionID, SessionFailed)
		return LabSession{}, fmt.Errorf("get lab runtime status: %w", err)
	}

	nextState := SessionStarting
	switch status.Phase {
	case RuntimeReady:
		if status.Ready {
			nextState = SessionReady
		}
	case RuntimeStopped:
		nextState = SessionStopped
	case RuntimeMissing, RuntimeFailed:
		nextState = SessionFailed
	}

	manager.setState(sessionID, nextState)
	return manager.snapshot(sessionID)
}

func (manager *SessionManager) Stop(ctx context.Context, sessionID string) error {
	manager.mu.Lock()
	session, ok := manager.sessions[sessionID]
	if !ok {
		manager.mu.Unlock()
		return nil
	}
	if session.State == SessionStopped || session.State == SessionStopping {
		manager.mu.Unlock()
		return nil
	}
	if session.State == SessionFailed && session.runtimeReference.PodName == "" {
		manager.mu.Unlock()
		return nil
	}
	session.State = SessionStopping
	session.UpdatedAt = manager.now()
	reference := session.runtimeReference
	manager.mu.Unlock()

	if err := manager.runtime.Stop(ctx, reference); err != nil {
		manager.setState(sessionID, SessionFailed)
		return fmt.Errorf("stop lab runtime: %w", err)
	}

	manager.setState(sessionID, SessionStopped)
	return nil
}

func (manager *SessionManager) snapshot(sessionID string) (LabSession, error) {
	manager.mu.RLock()
	defer manager.mu.RUnlock()

	session, ok := manager.sessions[sessionID]
	if !ok {
		return LabSession{}, errors.New("session not found")
	}
	return copySession(session), nil
}

func (manager *SessionManager) setState(sessionID string, state SessionState) {
	manager.mu.Lock()
	defer manager.mu.Unlock()

	if session, ok := manager.sessions[sessionID]; ok {
		session.State = state
		session.UpdatedAt = manager.now()
	}
}

func copySession(session *LabSession) LabSession {
	result := *session
	result.runtimeReference.Labels = cloneLabels(session.runtimeReference.Labels)
	return result
}

func validateLabIdentifier(value string) error {
	if len(value) == 0 || len(value) > 63 || !labIdentifierPattern.MatchString(value) {
		return fmt.Errorf("invalid lab identifier")
	}
	return nil
}

func newSessionID() (string, error) {
	var randomBytes [16]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", err
	}
	return "s-" + hex.EncodeToString(randomBytes[:]), nil
}
