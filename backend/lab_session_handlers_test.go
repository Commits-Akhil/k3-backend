package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeLabSessionService struct {
	createFn   func(context.Context, uuid.UUID, string) (LabSessionView, error)
	getFn      func(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error)
	listFn     func(context.Context, uuid.UUID, int, int) ([]LabSessionView, error)
	stopFn     func(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error)
	createID   uuid.UUID
	getID      uuid.UUID
	listID     uuid.UUID
	stopID     uuid.UUID
	listLimit  int
	listOffset int
}

func (service *fakeLabSessionService) CreateSession(ctx context.Context, userID uuid.UUID, labID string) (LabSessionView, error) {
	service.createID = userID
	return service.createFn(ctx, userID, labID)
}

func (service *fakeLabSessionService) GetSession(ctx context.Context, userID, sessionID uuid.UUID) (LabSessionView, error) {
	service.getID = userID
	return service.getFn(ctx, userID, sessionID)
}

func (service *fakeLabSessionService) ListUserSessions(ctx context.Context, userID uuid.UUID, limit, offset int) ([]LabSessionView, error) {
	service.listID, service.listLimit, service.listOffset = userID, limit, offset
	return service.listFn(ctx, userID, limit, offset)
}

func (service *fakeLabSessionService) StopSession(ctx context.Context, userID, sessionID uuid.UUID) (LabSessionView, error) {
	service.stopID = userID
	return service.stopFn(ctx, userID, sessionID)
}

func newTestLabSessionHandler(t *testing.T, service *fakeLabSessionService) (*LabSessionHandler, *JWTService) {
	t.Helper()
	handler, err := NewLabSessionHandler(service)
	if err != nil {
		t.Fatalf("NewLabSessionHandler returned error: %v", err)
	}
	return handler, newTestJWTService(t)
}

func authenticatedRequest(t *testing.T, method, target string, body string, jwtService *JWTService, userID uuid.UUID) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	token, err := jwtService.IssueAccessToken(userID)
	if err != nil {
		t.Fatalf("IssueAccessToken returned error: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	return request.WithContext(context.WithValue(request.Context(), authenticatedUserIDContextKey{}, userID))
}

func TestLabSessionRoutesRequireAuthentication(t *testing.T) {
	service := &fakeLabSessionService{
		createFn: func(context.Context, uuid.UUID, string) (LabSessionView, error) { return LabSessionView{}, nil },
		getFn:    func(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error) { return LabSessionView{}, nil },
		listFn:   func(context.Context, uuid.UUID, int, int) ([]LabSessionView, error) { return nil, nil },
		stopFn:   func(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error) { return LabSessionView{}, nil },
	}
	handler, jwtService := newTestLabSessionHandler(t, service)
	mux := http.NewServeMux()
	if err := RegisterLabSessionRoutes(mux, handler, jwtService); err != nil {
		t.Fatalf("RegisterLabSessionRoutes returned error: %v", err)
	}
	paths := []string{
		"/api/labs/sessions",
		"/api/labs/sessions/88888888-8888-8888-8888-888888888888",
		"/api/labs/sessions/88888888-8888-8888-8888-888888888888/stop",
	}
	for _, path := range paths {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want %d", path, response.Code, http.StatusUnauthorized)
		}
	}
}

func TestLabSessionCreateUsesAuthenticatedOwnerAndSafeResponse(t *testing.T) {
	ownerID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	view := testLabSessionView()
	service := &fakeLabSessionService{
		createFn: func(_ context.Context, userID uuid.UUID, labID string) (LabSessionView, error) {
			if userID != ownerID || labID != "intro-linux" {
				t.Fatalf("CreateSession arguments = %s, %q", userID, labID)
			}
			return view, nil
		},
		getFn:  func(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error) { return view, nil },
		listFn: func(context.Context, uuid.UUID, int, int) ([]LabSessionView, error) { return nil, nil },
		stopFn: func(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error) { return view, nil },
	}
	handler, jwtService := newTestLabSessionHandler(t, service)
	mux := http.NewServeMux()
	if err := RegisterLabSessionRoutes(mux, handler, jwtService); err != nil {
		t.Fatalf("RegisterLabSessionRoutes returned error: %v", err)
	}
	request := authenticatedRequest(t, http.MethodPost, "/api/labs/sessions", `{"lab_id":"intro-linux"}`, jwtService, ownerID)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || strings.Contains(response.Body.String(), "user_id") || strings.Contains(response.Body.String(), "runtime") {
		t.Fatalf("create response = %d %s", response.Code, response.Body.String())
	}
}

func TestLabSessionCreateRejectsInvalidJSONAndLabID(t *testing.T) {
	ownerID := uuid.New()
	service := &fakeLabSessionService{
		createFn: func(context.Context, uuid.UUID, string) (LabSessionView, error) {
			return LabSessionView{}, ErrInvalidInput
		},
		getFn:  func(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error) { return LabSessionView{}, nil },
		listFn: func(context.Context, uuid.UUID, int, int) ([]LabSessionView, error) { return nil, nil },
		stopFn: func(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error) { return LabSessionView{}, nil },
	}
	handler, jwtService := newTestLabSessionHandler(t, service)
	for name, body := range map[string]string{
		"malformed":     "{",
		"unknown field": `{"lab_id":"intro-linux","owner_id":"other"}`,
		"trailing JSON": `{"lab_id":"intro-linux"}{}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := authenticatedRequest(t, http.MethodPost, "/api/labs/sessions", body, jwtService, ownerID)
			response := httptest.NewRecorder()
			handler.create(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestLabSessionListUsesOwnerAndPagination(t *testing.T) {
	ownerID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	view := testLabSessionView()
	service := &fakeLabSessionService{
		createFn: func(context.Context, uuid.UUID, string) (LabSessionView, error) { return view, nil },
		getFn:    func(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error) { return view, nil },
		listFn: func(_ context.Context, userID uuid.UUID, limit, offset int) ([]LabSessionView, error) {
			if userID != ownerID || limit != 10 || offset != 20 {
				t.Fatalf("ListUserSessions arguments = %s, %d, %d", userID, limit, offset)
			}
			return []LabSessionView{view}, nil
		},
		stopFn: func(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error) { return view, nil },
	}
	handler, jwtService := newTestLabSessionHandler(t, service)
	request := authenticatedRequest(t, http.MethodGet, "/api/labs/sessions?limit=10&offset=20&user_id=99999999-9999-9999-9999-999999999999", "", jwtService, ownerID)
	response := httptest.NewRecorder()
	handler.list(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"limit":10`) || strings.Contains(response.Body.String(), "user_id") {
		t.Fatalf("list response = %d %s", response.Code, response.Body.String())
	}
}

func TestLabSessionListRejectsUnboundedPagination(t *testing.T) {
	service := &fakeLabSessionService{
		createFn: func(context.Context, uuid.UUID, string) (LabSessionView, error) { return LabSessionView{}, nil },
		getFn:    func(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error) { return LabSessionView{}, nil },
		listFn:   func(context.Context, uuid.UUID, int, int) ([]LabSessionView, error) { return nil, nil },
		stopFn:   func(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error) { return LabSessionView{}, nil },
	}
	handler, jwtService := newTestLabSessionHandler(t, service)
	request := authenticatedRequest(t, http.MethodGet, "/api/labs/sessions?limit=101", "", jwtService, uuid.New())
	response := httptest.NewRecorder()
	handler.list(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestLabSessionGetAndStopOwnershipAndIdempotency(t *testing.T) {
	ownerID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	sessionID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	view := testLabSessionView()
	view.ID = sessionID
	stopCalls := 0
	service := &fakeLabSessionService{
		createFn: func(context.Context, uuid.UUID, string) (LabSessionView, error) { return view, nil },
		getFn: func(_ context.Context, userID, id uuid.UUID) (LabSessionView, error) {
			if userID != ownerID || id != sessionID {
				return LabSessionView{}, ErrLabSessionNotFound
			}
			return view, nil
		},
		listFn: func(context.Context, uuid.UUID, int, int) ([]LabSessionView, error) { return nil, nil },
		stopFn: func(_ context.Context, userID, id uuid.UUID) (LabSessionView, error) {
			if userID != ownerID || id != sessionID {
				return LabSessionView{}, ErrLabSessionNotFound
			}
			stopCalls++
			view.State = SessionStopped
			return view, nil
		},
	}
	handler, jwtService := newTestLabSessionHandler(t, service)
	otherRequest := authenticatedRequest(t, http.MethodGet, "/api/labs/sessions/"+sessionID.String(), "", jwtService, uuid.New())
	otherResponse := httptest.NewRecorder()
	handler.get(otherResponse, otherRequest, sessionID.String())
	if otherResponse.Code != http.StatusNotFound {
		t.Fatalf("cross-owner status = %d, want %d", otherResponse.Code, http.StatusNotFound)
	}
	invalidRequest := authenticatedRequest(t, http.MethodGet, "/api/labs/sessions/not-a-uuid", "", jwtService, ownerID)
	invalidResponse := httptest.NewRecorder()
	handler.get(invalidResponse, invalidRequest, "not-a-uuid")
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid UUID status = %d, want %d", invalidResponse.Code, http.StatusBadRequest)
	}
	stopRequest := authenticatedRequest(t, http.MethodPost, "/api/labs/sessions/"+sessionID.String()+"/stop", "", jwtService, ownerID)
	stopResponse := httptest.NewRecorder()
	handler.stop(stopResponse, stopRequest, sessionID.String())
	if stopResponse.Code != http.StatusOK || stopCalls != 1 {
		t.Fatalf("stop response = %d, calls = %d", stopResponse.Code, stopCalls)
	}
}

func TestLabSessionHandlerMapsServiceFailureSafely(t *testing.T) {
	service := &fakeLabSessionService{
		createFn: func(context.Context, uuid.UUID, string) (LabSessionView, error) {
			return LabSessionView{}, errors.New("database DSN and runtime secret")
		},
		getFn:  func(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error) { return LabSessionView{}, nil },
		listFn: func(context.Context, uuid.UUID, int, int) ([]LabSessionView, error) { return nil, nil },
		stopFn: func(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error) { return LabSessionView{}, nil },
	}
	handler, jwtService := newTestLabSessionHandler(t, service)
	request := authenticatedRequest(t, http.MethodPost, "/api/labs/sessions", `{"lab_id":"intro-linux"}`, jwtService, uuid.New())
	response := httptest.NewRecorder()
	handler.create(response, request)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "DSN") || strings.Contains(response.Body.String(), "runtime secret") {
		t.Fatalf("failure response = %d %s", response.Code, response.Body.String())
	}
}

func TestLabSessionRoutesRegisterProtectedHandlers(t *testing.T) {
	service := &fakeLabSessionService{
		createFn: func(context.Context, uuid.UUID, string) (LabSessionView, error) { return testLabSessionView(), nil },
		getFn:    func(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error) { return testLabSessionView(), nil },
		listFn:   func(context.Context, uuid.UUID, int, int) ([]LabSessionView, error) { return nil, nil },
		stopFn:   func(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error) { return testLabSessionView(), nil },
	}
	handler, jwtService := newTestLabSessionHandler(t, service)
	mux := http.NewServeMux()
	if err := RegisterLabSessionRoutes(mux, handler, jwtService); err != nil {
		t.Fatalf("RegisterLabSessionRoutes returned error: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/labs/sessions/invalid", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("registered route status = %d, want middleware 401", response.Code)
	}
}

func testLabSessionView() LabSessionView {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	return LabSessionView{
		ID:        uuid.MustParse("55555555-5555-5555-5555-555555555555"),
		LabID:     "intro-linux",
		State:     SessionReady,
		CreatedAt: now,
		UpdatedAt: now,
		StartedAt: &now,
	}
}
