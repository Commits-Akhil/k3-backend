package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

type authenticatedUserIDContextKey struct{}

var ErrAuthenticatedIdentityMissing = errors.New("authenticated identity is missing")

func RequireAuthentication(service *JWTService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok || service == nil {
				writeUnauthorized(w)
				return
			}

			identity, err := service.ValidateAccessToken(token)
			if err != nil {
				writeUnauthorized(w)
				return
			}

			requestContext := context.WithValue(r.Context(), authenticatedUserIDContextKey{}, identity.UserID)
			next.ServeHTTP(w, r.WithContext(requestContext))
		})
	}
}

func AuthenticatedUserID(r *http.Request) (uuid.UUID, error) {
	if r == nil {
		return uuid.Nil, ErrAuthenticatedIdentityMissing
	}
	value := r.Context().Value(authenticatedUserIDContextKey{})
	userID, ok := value.(uuid.UUID)
	if !ok || userID == uuid.Nil {
		return uuid.Nil, ErrAuthenticatedIdentityMissing
	}
	return userID, nil
}

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func writeUnauthorized(w http.ResponseWriter) {
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
