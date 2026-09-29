package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

const maxAuthRequestBodyBytes int64 = 1 << 20

type accountService interface {
	RegisterStudent(context.Context, RegisterStudentInput) (PublicUser, error)
	Authenticate(context.Context, AuthenticateInput) (PublicUser, error)
	GetByID(context.Context, uuid.UUID) (PublicUser, error)
}

type accessTokenIssuer interface {
	IssueAccessToken(uuid.UUID) (string, error)
	AccessTokenLifetime() time.Duration
}

type AuthHandler struct {
	accounts accountService
	tokens   accessTokenIssuer
	now      func() time.Time
}

type authCredentialsRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type userResponse struct {
	User PublicUser `json:"user"`
}

type loginResponse struct {
	User        PublicUser `json:"user"`
	AccessToken string     `json:"access_token"`
	TokenType   string     `json:"token_type"`
	ExpiresIn   int64      `json:"expires_in"`
	ExpiresAt   time.Time  `json:"expires_at"`
}

type apiErrorResponse struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func NewAuthHandler(accounts accountService, tokens accessTokenIssuer) (*AuthHandler, error) {
	if accounts == nil || tokens == nil {
		return nil, errors.New("authentication dependencies are required")
	}
	return &AuthHandler{
		accounts: accounts,
		tokens:   tokens,
		now:      time.Now,
	}, nil
}

func RegisterAuthRoutes(mux *http.ServeMux, handler *AuthHandler, jwtService *JWTService) error {
	if mux == nil || handler == nil || jwtService == nil {
		return errors.New("authentication route dependencies are required")
	}
	mux.HandleFunc("/api/auth/register", handler.Register)
	mux.HandleFunc("/api/auth/login", handler.Login)
	mux.Handle("/api/auth/me", RequireAuthentication(jwtService)(http.HandlerFunc(handler.Me)))
	return nil
}

func (handler *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}

	var request authCredentialsRequest
	if err := decodeAuthJSON(w, r, &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid request")
		return
	}

	user, err := handler.accounts.RegisterStudent(r.Context(), RegisterStudentInput{
		Email:    request.Email,
		Password: request.Password,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput):
			writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid request")
		case errors.Is(err, ErrDuplicateEmail):
			writeAPIError(w, http.StatusConflict, "email_exists", "email already exists")
		default:
			writeAPIError(w, http.StatusInternalServerError, "server_error", "request could not be completed")
		}
		return
	}

	writeJSON(w, http.StatusCreated, userResponse{User: user})
}

func (handler *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}

	var request authCredentialsRequest
	if err := decodeAuthJSON(w, r, &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid request")
		return
	}

	user, err := handler.accounts.Authenticate(r.Context(), AuthenticateInput{
		Email:    request.Email,
		Password: request.Password,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidCredentials), errors.Is(err, ErrAccountDisabled):
			writeAPIError(w, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
		default:
			writeAPIError(w, http.StatusInternalServerError, "server_error", "request could not be completed")
		}
		return
	}

	token, err := handler.tokens.IssueAccessToken(user.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "request could not be completed")
		return
	}
	lifetime := handler.tokens.AccessTokenLifetime()
	issuedAt := handler.now().UTC()
	writeJSON(w, http.StatusOK, loginResponse{
		User:        user,
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresIn:   int64(lifetime.Seconds()),
		ExpiresAt:   issuedAt.Add(lifetime),
	})
}

func (handler *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}

	userID, err := AuthenticatedUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return
	}
	user, err := handler.accounts.GetByID(r.Context(), userID)
	if err != nil {
		switch {
		case errors.Is(err, ErrUserNotFound), errors.Is(err, ErrAccountDisabled), errors.Is(err, ErrInvalidInput):
			writeAPIError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		default:
			writeAPIError(w, http.StatusInternalServerError, "server_error", "request could not be completed")
		}
		return
	}
	if user.Status != UserStatusActive {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return
	}

	writeJSON(w, http.StatusOK, userResponse{User: user})
}

func decodeAuthJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	if r.Body == nil {
		return ErrInvalidInput
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxAuthRequestBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return ErrInvalidInput
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrInvalidInput
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiErrorResponse{Error: apiError{Code: code, Message: message}})
}

func writeMethodNotAllowed(w http.ResponseWriter, method string) {
	w.Header().Set("Allow", method)
	writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}
