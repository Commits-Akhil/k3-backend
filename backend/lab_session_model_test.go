package main

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLabSessionRecordValidationAndTransitions(t *testing.T) {
	session := testLabSession()
	if err := validateLabSessionRecord(session); err != nil {
		t.Fatalf("valid session returned error: %v", err)
	}

	invalid := session
	invalid.LabID = "Invalid Lab"
	if !errors.Is(validateLabSessionRecord(invalid), ErrInvalidInput) {
		t.Fatal("invalid lab ID was accepted")
	}
	invalid = session
	invalid.State = SessionState("UNKNOWN")
	if !errors.Is(validateLabSessionRecord(invalid), ErrInvalidInput) {
		t.Fatal("invalid state was accepted")
	}

	transitions := []struct {
		current SessionState
		next    SessionState
		valid   bool
	}{
		{SessionRequested, SessionStarting, true},
		{SessionStarting, SessionReady, true},
		{SessionReady, SessionStopping, true},
		{SessionStopping, SessionStopped, true},
		{SessionStopped, SessionReady, false},
		{SessionRequested, SessionStopped, false},
	}
	for _, transition := range transitions {
		if got := validStateTransition(transition.current, transition.next); got != transition.valid {
			t.Errorf("transition %s -> %s = %v, want %v", transition.current, transition.next, got, transition.valid)
		}
	}
}

func TestLabSessionRecordRequiresCompleteRuntimeReferences(t *testing.T) {
	session := testLabSession()
	value := "cyber-labs"
	session.RuntimeNamespace = &value
	if !errors.Is(validateLabSessionRecord(session), ErrInvalidInput) {
		t.Fatal("partial runtime reference was accepted")
	}
}

func testLabSession() LabSessionRecord {
	createdAt := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	return LabSessionRecord{
		ID:        uuid.MustParse("88888888-8888-8888-8888-888888888888"),
		UserID:    uuid.MustParse("99999999-9999-9999-9999-999999999999"),
		LabID:     "intro-linux",
		State:     SessionRequested,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
}
