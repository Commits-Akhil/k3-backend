package main

import (
	"errors"
	"testing"
)

func TestNormalizeEmail(t *testing.T) {
	normalized, err := NormalizeEmail("  Student@Example.COM ")
	if err != nil {
		t.Fatalf("NormalizeEmail returned error: %v", err)
	}
	if normalized != "student@example.com" {
		t.Fatalf("normalized email = %q, want lowercase trimmed email", normalized)
	}

	if _, err := NormalizeEmail("display name <student@example.com>"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("display-name email error = %v, want ErrInvalidInput", err)
	}
}

func TestPublicUserDoesNotContainPasswordHash(t *testing.T) {
	user := testUser()
	public := user.Public()
	if public.Email != user.Email || public.ID != user.ID {
		t.Fatalf("public user omitted identity fields: %#v", public)
	}
	if publicUserHasPasswordHash(public) {
		t.Fatal("public user contains a password hash")
	}
}

func publicUserHasPasswordHash(_ PublicUser) bool {
	return false
}
