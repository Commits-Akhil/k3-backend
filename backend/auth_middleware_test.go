package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestRequireAuthenticationAddsUserIDToContext(t *testing.T) {
	service := newTestJWTService(t)
	userID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	token, err := service.IssueAccessToken(userID)
	if err != nil {
		t.Fatalf("IssueAccessToken returned error: %v", err)
	}

	handler := RequireAuthentication(service)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := AuthenticatedUserID(r)
		if err != nil {
			t.Fatalf("AuthenticatedUserID returned error: %v", err)
		}
		if got != userID {
			t.Fatalf("authenticated user ID = %s, want %s", got, userID)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodGet, "/protected", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestRequireAuthenticationRejectsMissingMalformedAndInvalidTokens(t *testing.T) {
	service := newTestJWTService(t)
	invalidToken, err := service.IssueAccessToken(uuid.MustParse("66666666-6666-6666-6666-666666666666"))
	if err != nil {
		t.Fatalf("IssueAccessToken returned error: %v", err)
	}

	for _, header := range []string{"", "Bearer", "Basic token", "Bearer one two", "Bearer not.a.jwt", "bearer"} {
		t.Run(header, func(t *testing.T) {
			handler := RequireAuthentication(service)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("protected handler was called for invalid authorization")
			}))
			request := httptest.NewRequest(http.MethodGet, "/protected", nil)
			if header == "Bearer not.a.jwt" {
				request.Header.Set("Authorization", "Bearer "+invalidToken+".")
			} else {
				request.Header.Set("Authorization", header)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("response status = %d, want %d", response.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestAuthenticatedUserIDRejectsMissingIdentity(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/protected", nil)
	if _, err := AuthenticatedUserID(request); !errors.Is(err, ErrAuthenticatedIdentityMissing) {
		t.Fatalf("missing identity error = %v, want ErrAuthenticatedIdentityMissing", err)
	}
	if _, err := AuthenticatedUserID(nil); !errors.Is(err, ErrAuthenticatedIdentityMissing) {
		t.Fatalf("nil request error = %v, want ErrAuthenticatedIdentityMissing", err)
	}
}

func TestBearerTokenParsing(t *testing.T) {
	if token, ok := bearerToken("bearer token-value"); !ok || token != "token-value" {
		t.Fatalf("case-insensitive bearer parsing = %q, %v", token, ok)
	}
	if _, ok := bearerToken("Bearer token-value extra"); ok {
		t.Fatal("malformed bearer header was accepted")
	}
}
