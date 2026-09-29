package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeAccountService struct {
	registerFn      func(context.Context, RegisterStudentInput) (PublicUser, error)
	authenticateFn  func(context.Context, AuthenticateInput) (PublicUser, error)
	getByIDFn       func(context.Context, uuid.UUID) (PublicUser, error)
	requestedUserID uuid.UUID
}

func (service *fakeAccountService) RegisterStudent(ctx context.Context, input RegisterStudentInput) (PublicUser, error) {
	return service.registerFn(ctx, input)
}

func (service *fakeAccountService) Authenticate(ctx context.Context, input AuthenticateInput) (PublicUser, error) {
	return service.authenticateFn(ctx, input)
}

func (service *fakeAccountService) GetByID(ctx context.Context, id uuid.UUID) (PublicUser, error) {
	service.requestedUserID = id
	return service.getByIDFn(ctx, id)
}

type fakeAccessTokenIssuer struct {
	token    string
	lifetime time.Duration
	err      error
	userID   uuid.UUID
}

func (issuer *fakeAccessTokenIssuer) IssueAccessToken(userID uuid.UUID) (string, error) {
	issuer.userID = userID
	if issuer.err != nil {
		return "", issuer.err
	}
	return issuer.token, nil
}

func (issuer *fakeAccessTokenIssuer) AccessTokenLifetime() time.Duration {
	return issuer.lifetime
}

func newTestAuthHandler(t *testing.T, accounts *fakeAccountService, tokens accessTokenIssuer) *AuthHandler {
	t.Helper()
	handler, err := NewAuthHandler(accounts, tokens)
	if err != nil {
		t.Fatalf("NewAuthHandler returned error: %v", err)
	}
	handler.now = func() time.Time {
		return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	}
	return handler
}

func TestAuthHandlerRegisterReturnsCreatedPublicStudent(t *testing.T) {
	user := testUser().Public()
	accounts := &fakeAccountService{
		registerFn: func(_ context.Context, input RegisterStudentInput) (PublicUser, error) {
			if input.Email != "student@example.com" || input.Password != "test-password" {
				t.Fatalf("registration input = %#v, want normalized credentials", input)
			}
			return user, nil
		},
	}
	handler := newTestAuthHandler(t, accounts, &fakeAccessTokenIssuer{})

	request := httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(`{"email":"student@example.com","password":"test-password"}`))
	response := httptest.NewRecorder()
	handler.Register(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusCreated)
	}
	var body map[string]any
	decodeResponse(t, response, &body)
	if _, ok := body["user"].(map[string]any); !ok {
		t.Fatalf("response = %#v, missing user", body)
	}
	if strings.Contains(response.Body.String(), "password_hash") {
		t.Fatal("registration response contains password hash")
	}
}

func TestAuthHandlerRegisterRejectsRoleAndMapsDuplicate(t *testing.T) {
	accounts := &fakeAccountService{
		registerFn: func(context.Context, RegisterStudentInput) (PublicUser, error) {
			return PublicUser{}, ErrDuplicateEmail
		},
	}
	handler := newTestAuthHandler(t, accounts, &fakeAccessTokenIssuer{})

	roleRequest := httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(`{"email":"student@example.com","password":"test-password","role":"admin"}`))
	roleResponse := httptest.NewRecorder()
	handler.Register(roleResponse, roleRequest)
	if roleResponse.Code != http.StatusBadRequest {
		t.Fatalf("role request status = %d, want %d", roleResponse.Code, http.StatusBadRequest)
	}

	duplicateRequest := httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(`{"email":"student@example.com","password":"test-password"}`))
	duplicateResponse := httptest.NewRecorder()
	handler.Register(duplicateResponse, duplicateRequest)
	if duplicateResponse.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want %d", duplicateResponse.Code, http.StatusConflict)
	}
}

func TestAuthHandlerRegisterRejectsMalformedAndInvalidInput(t *testing.T) {
	accounts := &fakeAccountService{
		registerFn: func(context.Context, RegisterStudentInput) (PublicUser, error) {
			return PublicUser{}, ErrInvalidInput
		},
	}
	handler := newTestAuthHandler(t, accounts, &fakeAccessTokenIssuer{})

	for name, body := range map[string]string{
		"malformed JSON":        "{",
		"invalid service input": `{"email":"bad","password":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.Register(response, httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(body)))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestAuthHandlerLoginReturnsJWTAndPublicUser(t *testing.T) {
	user := testUser().Public()
	jwtService := newTestJWTService(t)
	accounts := &fakeAccountService{
		authenticateFn: func(_ context.Context, input AuthenticateInput) (PublicUser, error) {
			if input.Email != "student@example.com" || input.Password != "test-password" {
				t.Fatalf("authentication input = %#v", input)
			}
			return user, nil
		},
	}
	handler := newTestAuthHandler(t, accounts, jwtService)

	response := httptest.NewRecorder()
	handler.Login(response, httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"student@example.com","password":"test-password"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	var body loginResponse
	decodeResponse(t, response, &body)
	if body.AccessToken == "" || body.TokenType != "Bearer" || body.ExpiresIn != int64(defaultAccessTokenTTL.Seconds()) {
		t.Fatalf("login response = %#v", body)
	}
	if _, err := jwtService.ValidateAccessToken(body.AccessToken); err != nil {
		t.Fatalf("returned access token did not validate: %v", err)
	}
	if strings.Contains(response.Body.String(), "password_hash") {
		t.Fatal("login response contains password hash")
	}
}

func TestAuthHandlerLoginUsesGenericUnauthorizedErrors(t *testing.T) {
	for name, serviceError := range map[string]error{
		"invalid credentials": ErrInvalidCredentials,
		"disabled account":    ErrAccountDisabled,
	} {
		t.Run(name, func(t *testing.T) {
			accounts := &fakeAccountService{
				authenticateFn: func(context.Context, AuthenticateInput) (PublicUser, error) {
					return PublicUser{}, serviceError
				},
			}
			handler := newTestAuthHandler(t, accounts, &fakeAccessTokenIssuer{})
			response := httptest.NewRecorder()
			handler.Login(response, httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"student@example.com","password":"test-password"}`)))
			if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), serviceError.Error()) {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestAuthHandlerSanitizesInternalFailures(t *testing.T) {
	accounts := &fakeAccountService{
		registerFn: func(context.Context, RegisterStudentInput) (PublicUser, error) {
			return PublicUser{}, errors.New("database DSN and password hash must not escape")
		},
	}
	handler := newTestAuthHandler(t, accounts, &fakeAccessTokenIssuer{})
	response := httptest.NewRecorder()
	handler.Register(response, httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(`{"email":"student@example.com","password":"test-password"}`)))
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "DSN") || strings.Contains(response.Body.String(), "password hash") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestAuthHandlerMeUsesAuthenticatedIdentityOnly(t *testing.T) {
	user := testUser().Public()
	service := newTestJWTService(t)
	accounts := &fakeAccountService{
		getByIDFn: func(_ context.Context, id uuid.UUID) (PublicUser, error) {
			if id != user.ID {
				t.Fatalf("/me requested user ID %s, want token identity %s", id, user.ID)
			}
			return user, nil
		},
	}
	handler := newTestAuthHandler(t, accounts, service)
	mux := http.NewServeMux()
	if err := RegisterAuthRoutes(mux, handler, service); err != nil {
		t.Fatalf("RegisterAuthRoutes returned error: %v", err)
	}
	token, err := service.IssueAccessToken(user.ID)
	if err != nil {
		t.Fatalf("IssueAccessToken returned error: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/auth/me?user_id=77777777-7777-7777-7777-777777777777", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "password_hash") {
		t.Fatalf("/me response = %d %s", response.Code, response.Body.String())
	}
}

func TestAuthHandlerMeRejectsMissingInvalidAndDisabledIdentity(t *testing.T) {
	user := testUser().Public()
	service := newTestJWTService(t)
	accounts := &fakeAccountService{
		getByIDFn: func(context.Context, uuid.UUID) (PublicUser, error) {
			return user, nil
		},
	}
	handler := newTestAuthHandler(t, accounts, service)
	mux := http.NewServeMux()
	if err := RegisterAuthRoutes(mux, handler, service); err != nil {
		t.Fatalf("RegisterAuthRoutes returned error: %v", err)
	}

	for name, token := range map[string]string{"missing": "", "invalid": "not.a.jwt"} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
			if token != "" {
				request.Header.Set("Authorization", "Bearer "+token)
			}
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
			}
		})
	}

	disabled := user
	disabled.Status = UserStatusDisabled
	accounts.getByIDFn = func(context.Context, uuid.UUID) (PublicUser, error) { return disabled, nil }
	token, err := service.IssueAccessToken(user.ID)
	if err != nil {
		t.Fatalf("IssueAccessToken returned error: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/auth/me", bytes.NewReader([]byte("ignored")))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("disabled status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, destination any) {
	t.Helper()
	if err := json.NewDecoder(response.Body).Decode(destination); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}
