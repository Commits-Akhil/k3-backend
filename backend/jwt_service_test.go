package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const testJWTSecret = "stage-three-test-signing-key-with-enough-bytes"

func newTestJWTService(t *testing.T) *JWTService {
	t.Helper()
	service, err := NewJWTService(JWTConfig{SigningSecret: testJWTSecret})
	if err != nil {
		t.Fatalf("NewJWTService returned error: %v", err)
	}
	service.now = func() time.Time {
		return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	}
	return service
}

func TestJWTServiceIssuesAndValidatesAccessToken(t *testing.T) {
	service := newTestJWTService(t)
	userID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	tokenString, err := service.IssueAccessToken(userID)
	if err != nil {
		t.Fatalf("IssueAccessToken returned error: %v", err)
	}

	identity, err := service.ValidateAccessToken(tokenString)
	if err != nil {
		t.Fatalf("ValidateAccessToken returned error: %v", err)
	}
	if identity.UserID != userID {
		t.Fatalf("validated user ID = %s, want %s", identity.UserID, userID)
	}

	claims := &jwt.RegisteredClaims{}
	parsed, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (any, error) {
		return []byte(testJWTSecret), nil
	}, jwt.WithTimeFunc(service.now))
	if err != nil || !parsed.Valid {
		t.Fatalf("parsing issued claims returned %v, %v", parsed, err)
	}
	if claims.Subject != userID.String() || claims.Issuer != jwtIssuer || len(claims.Audience) != 1 || claims.Audience[0] != jwtAudience {
		t.Fatalf("issued claims = %#v", claims)
	}
	if !claims.IssuedAt.Time.Equal(service.now()) || !claims.ExpiresAt.Time.Equal(service.now().Add(defaultAccessTokenTTL)) {
		t.Fatalf("issued timestamps = %#v", claims)
	}
	if parsed.Method != jwt.SigningMethodHS256 {
		t.Fatalf("signing method = %s, want HS256", parsed.Method.Alg())
	}
}

func TestJWTServiceRejectsInvalidTokens(t *testing.T) {
	service := newTestJWTService(t)
	userID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	issuedAt := service.now()

	tests := map[string]string{
		"expired": makeSignedTestToken(t, testJWTSecret, jwt.SigningMethodHS256, jwt.RegisteredClaims{
			Subject: userID.String(), Issuer: jwtIssuer, Audience: jwt.ClaimStrings{jwtAudience},
			IssuedAt: jwt.NewNumericDate(issuedAt.Add(-time.Hour)), ExpiresAt: jwt.NewNumericDate(issuedAt.Add(-time.Minute)),
		}),
		"malformed": "not.a.jwt",
		"wrong signature": makeSignedTestToken(t, "different-test-signing-key-with-enough-bytes", jwt.SigningMethodHS256, jwt.RegisteredClaims{
			Subject: userID.String(), Issuer: jwtIssuer, Audience: jwt.ClaimStrings{jwtAudience},
			IssuedAt: jwt.NewNumericDate(issuedAt), ExpiresAt: jwt.NewNumericDate(issuedAt.Add(defaultAccessTokenTTL)),
		}),
		"wrong algorithm": makeSignedTestToken(t, testJWTSecret, jwt.SigningMethodHS384, jwt.RegisteredClaims{
			Subject: userID.String(), Issuer: jwtIssuer, Audience: jwt.ClaimStrings{jwtAudience},
			IssuedAt: jwt.NewNumericDate(issuedAt), ExpiresAt: jwt.NewNumericDate(issuedAt.Add(defaultAccessTokenTTL)),
		}),
		"wrong issuer": makeSignedTestToken(t, testJWTSecret, jwt.SigningMethodHS256, jwt.RegisteredClaims{
			Subject: userID.String(), Issuer: "different-issuer", Audience: jwt.ClaimStrings{jwtAudience},
			IssuedAt: jwt.NewNumericDate(issuedAt), ExpiresAt: jwt.NewNumericDate(issuedAt.Add(defaultAccessTokenTTL)),
		}),
		"wrong audience": makeSignedTestToken(t, testJWTSecret, jwt.SigningMethodHS256, jwt.RegisteredClaims{
			Subject: userID.String(), Issuer: jwtIssuer, Audience: jwt.ClaimStrings{"different-audience"},
			IssuedAt: jwt.NewNumericDate(issuedAt), ExpiresAt: jwt.NewNumericDate(issuedAt.Add(defaultAccessTokenTTL)),
		}),
		"missing subject": makeSignedTestToken(t, testJWTSecret, jwt.SigningMethodHS256, jwt.RegisteredClaims{
			Issuer: jwtIssuer, Audience: jwt.ClaimStrings{jwtAudience},
			IssuedAt: jwt.NewNumericDate(issuedAt), ExpiresAt: jwt.NewNumericDate(issuedAt.Add(defaultAccessTokenTTL)),
		}),
	}

	for name, tokenString := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := service.ValidateAccessToken(tokenString)
			if !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("ValidateAccessToken error = %v, want ErrInvalidToken", err)
			}
			if strings.Contains(err.Error(), testJWTSecret) {
				t.Fatal("signing secret appeared in token validation error")
			}
		})
	}
}

func makeSignedTestToken(t *testing.T, secret string, method jwt.SigningMethod, claims jwt.RegisteredClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	serialized, err := token.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("SignedString returned error: %v", err)
	}
	return serialized
}
