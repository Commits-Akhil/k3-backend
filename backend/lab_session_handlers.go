package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

type labSessionService interface {
	CreateSession(context.Context, uuid.UUID, string) (LabSessionView, error)
	GetSession(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error)
	ListUserSessions(context.Context, uuid.UUID, int, int) ([]LabSessionView, error)
	StopSession(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error)
}

type LabSessionHandler struct {
	service        labSessionService
	terminalTicket *TerminalHandler
}

type createLabSessionRequest struct {
	LabID string `json:"lab_id"`
}

type labSessionListResponse struct {
	Sessions []LabSessionView `json:"sessions"`
	Limit    int              `json:"limit"`
	Offset   int              `json:"offset"`
}

func NewLabSessionHandler(service labSessionService) (*LabSessionHandler, error) {
	if service == nil {
		return nil, errors.New("lab session service is required")
	}
	return &LabSessionHandler{service: service}, nil
}

func RegisterLabSessionRoutes(mux *http.ServeMux, handler *LabSessionHandler, jwtService *JWTService) error {
	if mux == nil || handler == nil || jwtService == nil {
		return errors.New("lab session route dependencies are required")
	}
	authenticated := RequireAuthentication(jwtService)
	mux.Handle("/api/labs/sessions", authenticated(http.HandlerFunc(handler.collection)))
	mux.Handle("/api/labs/sessions/", authenticated(http.HandlerFunc(handler.member)))
	return nil
}

func (handler *LabSessionHandler) collection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		handler.create(w, r)
	case http.MethodGet:
		handler.list(w, r)
	default:
		writeMethodNotAllowed(w, http.MethodPost+", "+http.MethodGet)
	}
}

func (handler *LabSessionHandler) member(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/labs/sessions/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 1 && parts[0] != "" {
		handler.get(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "stop" && parts[0] != "" {
		handler.stop(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "terminal-ticket" && parts[0] != "" && handler.terminalTicket != nil {
		handler.terminalTicket.IssueTicket(w, r, parts[0])
		return
	}
	writeAPIError(w, http.StatusNotFound, "not_found", "not found")
}

func (handler *LabSessionHandler) create(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	userID, err := AuthenticatedUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return
	}
	var request createLabSessionRequest
	if err := decodeAuthJSON(w, r, &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid request")
		return
	}
	view, err := handler.service.CreateSession(r.Context(), userID, request.LabID)
	if err != nil {
		handler.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (handler *LabSessionHandler) list(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	userID, err := AuthenticatedUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return
	}
	limit, offset, err := parseSessionPagination(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid request")
		return
	}
	sessions, err := handler.service.ListUserSessions(r.Context(), userID, limit, offset)
	if err != nil {
		handler.writeServiceError(w, err)
		return
	}
	if limit == 0 {
		limit = defaultSessionPageSize
	}
	writeJSON(w, http.StatusOK, labSessionListResponse{Sessions: sessions, Limit: limit, Offset: offset})
}

func (handler *LabSessionHandler) get(w http.ResponseWriter, r *http.Request, rawID string) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	userID, sessionID, ok := handler.requestIdentity(w, r, rawID)
	if !ok {
		return
	}
	view, err := handler.service.GetSession(r.Context(), userID, sessionID)
	if err != nil {
		handler.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (handler *LabSessionHandler) stop(w http.ResponseWriter, r *http.Request, rawID string) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	userID, sessionID, ok := handler.requestIdentity(w, r, rawID)
	if !ok {
		return
	}
	view, err := handler.service.StopSession(r.Context(), userID, sessionID)
	if err != nil {
		handler.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (handler *LabSessionHandler) requestIdentity(w http.ResponseWriter, r *http.Request, rawID string) (uuid.UUID, uuid.UUID, bool) {
	userID, err := AuthenticatedUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return uuid.Nil, uuid.Nil, false
	}
	sessionID, err := uuid.Parse(rawID)
	if err != nil || sessionID == uuid.Nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid request")
		return uuid.Nil, uuid.Nil, false
	}
	return userID, sessionID, true
}

func (handler *LabSessionHandler) writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid request")
	case errors.Is(err, ErrLabSessionNotFound):
		writeAPIError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, ErrSessionNotReady):
		writeAPIError(w, http.StatusConflict, "not_ready", "lab session is not ready")
	default:
		writeAPIError(w, http.StatusInternalServerError, "server_error", "request could not be completed")
	}
}

func parseSessionPagination(r *http.Request) (int, int, error) {
	limit, offset := 0, 0
	query := r.URL.Query()
	if value := query.Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > maxSessionPageSize {
			return 0, 0, ErrInvalidInput
		}
		limit = parsed
	}
	if value := query.Get("offset"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			return 0, 0, ErrInvalidInput
		}
		offset = parsed
	}
	return limit, offset, nil
}
