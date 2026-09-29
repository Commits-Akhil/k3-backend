package main

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	defaultSessionPageSize = 50
	maxSessionPageSize     = 100
)

var (
	ErrLabSessionNotFound     = errors.New("lab session not found")
	ErrInvalidStateTransition = errors.New("invalid lab session state transition")
)

// LabSessionRecord is the persistence model for the lab_sessions table.
// Runtime references are backend-owned and are never client credentials.
type LabSessionRecord struct {
	ID                 uuid.UUID
	UserID             uuid.UUID
	LabID              string
	State              SessionState
	RuntimeNamespace   *string
	RuntimePodName     *string
	RuntimeServiceName *string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	StartedAt          *time.Time
	StoppedAt          *time.Time
	FailureCode        *string
}

func validateLabSessionRecord(session LabSessionRecord) error {
	if session.ID == uuid.Nil || session.UserID == uuid.Nil || session.CreatedAt.IsZero() || session.UpdatedAt.IsZero() {
		return ErrInvalidInput
	}
	if err := validateLabIdentifier(session.LabID); err != nil {
		return ErrInvalidInput
	}
	if !validSessionState(session.State) {
		return ErrInvalidInput
	}
	if err := validateOptionalRuntimeReference(session.RuntimeNamespace, session.RuntimePodName, session.RuntimeServiceName); err != nil {
		return err
	}
	return nil
}

func validSessionState(state SessionState) bool {
	switch state {
	case SessionRequested, SessionStarting, SessionReady, SessionStopping, SessionStopped, SessionFailed:
		return true
	default:
		return false
	}
}

func validateOptionalRuntimeReference(namespace, podName, serviceName *string) error {
	if namespace == nil && podName == nil && serviceName == nil {
		return nil
	}
	if namespace == nil || podName == nil || serviceName == nil || *namespace == "" || *podName == "" || *serviceName == "" {
		return ErrInvalidInput
	}
	return nil
}

func validStateTransition(current, next SessionState) bool {
	if current == next {
		return true
	}
	switch current {
	case SessionRequested:
		return next == SessionStarting || next == SessionFailed
	case SessionStarting:
		return next == SessionReady || next == SessionStopping || next == SessionFailed
	case SessionReady:
		return next == SessionStopping || next == SessionFailed
	case SessionStopping:
		return next == SessionStopped || next == SessionFailed
	case SessionFailed:
		return next == SessionStopping || next == SessionStopped
	case SessionStopped:
		return false
	default:
		return false
	}
}
