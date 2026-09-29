package main

import (
	"errors"
	"strings"
	"testing"
)

func TestPasswordHashAndVerification(t *testing.T) {
	password := "test-password-for-unit-tests"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword returned error: %v", err)
	}
	if hash == password {
		t.Fatal("password hash equals plaintext password")
	}

	valid, err := VerifyPassword(password, hash)
	if err != nil || !valid {
		t.Fatalf("VerifyPassword(valid password) = %v, %v", valid, err)
	}
	valid, err = VerifyPassword("wrong-password", hash)
	if err != nil || valid {
		t.Fatalf("VerifyPassword(wrong password) = %v, %v", valid, err)
	}
}

func TestPasswordValidation(t *testing.T) {
	for _, password := range []string{"", strings.Repeat("x", maxPasswordBytes+1)} {
		if _, err := HashPassword(password); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("HashPassword(%d bytes) error = %v, want ErrInvalidInput", len(password), err)
		}
	}

	hash := "not-a-valid-hash"
	valid, err := VerifyPassword("test-password-for-unit-tests", hash)
	if err != nil || valid {
		t.Fatalf("VerifyPassword(invalid hash) = %v, %v", valid, err)
	}
}

func TestPasswordErrorsDoNotContainHash(t *testing.T) {
	hash, err := HashPassword("test-password-for-unit-tests")
	if err != nil {
		t.Fatalf("HashPassword returned error: %v", err)
	}
	valid, verifyErr := VerifyPassword("", hash)
	if valid || !errors.Is(verifyErr, ErrInvalidInput) {
		t.Fatalf("VerifyPassword(empty password) = %v, %v", valid, verifyErr)
	}
	if strings.Contains(verifyErr.Error(), hash) {
		t.Fatal("password hash appeared in verification error")
	}
}
