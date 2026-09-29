package main

import (
	"context"
	"sync"
	"testing"
)

func TestSessionManagerTransitionsAndStopsIdempotently(t *testing.T) {
	runtime := &fakeLabRuntime{}
	manager, err := NewSessionManager(runtime)
	if err != nil {
		t.Fatalf("NewSessionManager returned error: %v", err)
	}

	session, err := manager.Create(context.Background(), "future-user", "intro-linux")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if session.State != SessionStarting {
		t.Fatalf("created session state = %q, want %q", session.State, SessionStarting)
	}
	if session.ID == "" || session.UserID != "future-user" {
		t.Fatalf("session identity = %#v", session)
	}

	session, err = manager.Get(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if session.State != SessionReady {
		t.Fatalf("session state after ready status = %q, want %q", session.State, SessionReady)
	}

	if err := manager.Stop(context.Background(), session.ID); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if err := manager.Stop(context.Background(), session.ID); err != nil {
		t.Fatalf("second Stop returned error: %v", err)
	}
	runtime.mu.Lock()
	stopCount := runtime.stopped
	runtime.mu.Unlock()
	if stopCount != 1 {
		t.Fatalf("runtime Stop count = %d, want 1", stopCount)
	}
}

func TestSessionManagerRejectsInvalidLabIDAndRecordsCreateFailure(t *testing.T) {
	runtime := &fakeLabRuntime{createErr: context.Canceled}
	manager, err := NewSessionManager(runtime)
	if err != nil {
		t.Fatalf("NewSessionManager returned error: %v", err)
	}

	if _, err := manager.Create(context.Background(), "user", "Invalid Lab"); err == nil {
		t.Fatal("invalid lab ID was accepted")
	}
	if _, err := manager.Create(context.Background(), "user", "intro-linux"); err == nil {
		t.Fatal("runtime creation failure was not returned")
	}
}

func TestSessionManagerSupportsConcurrentAccess(t *testing.T) {
	manager, err := NewSessionManager(&fakeLabRuntime{})
	if err != nil {
		t.Fatalf("NewSessionManager returned error: %v", err)
	}

	const sessionCount = 32
	ids := make(chan string, sessionCount)
	var waitGroup sync.WaitGroup
	waitGroup.Add(sessionCount)
	for index := 0; index < sessionCount; index++ {
		go func() {
			defer waitGroup.Done()
			session, createErr := manager.Create(context.Background(), "user", "intro-linux")
			if createErr == nil {
				ids <- session.ID
			}
		}()
	}
	waitGroup.Wait()
	close(ids)

	created := 0
	for range ids {
		created++
	}
	if created != sessionCount {
		t.Fatalf("created sessions = %d, want %d", created, sessionCount)
	}
}
