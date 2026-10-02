package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type fakeGUIService struct {
	reference RuntimeReference
	err       error
}

func (service *fakeGUIService) GetTerminalReference(context.Context, uuid.UUID, uuid.UUID) (RuntimeReference, error) {
	return service.reference, service.err
}
func (service *fakeGUIService) CreateSession(context.Context, uuid.UUID, string) (LabSessionView, error) {
	return LabSessionView{}, nil
}
func (service *fakeGUIService) GetSession(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error) {
	return LabSessionView{}, nil
}
func (service *fakeGUIService) ListUserSessions(context.Context, uuid.UUID, int, int) ([]LabSessionView, error) {
	return nil, nil
}
func (service *fakeGUIService) StopSession(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error) {
	return LabSessionView{}, nil
}

type fakeGUIResolver struct {
	target guiProxyTarget
	err    error
	seen   RuntimeReference
}

func (resolver *fakeGUIResolver) ResolveGUIProxyTarget(_ context.Context, reference RuntimeReference) (guiProxyTarget, error) {
	resolver.seen = reference
	return resolver.target, resolver.err
}

func newGUIHandlerForTest(t *testing.T, service *fakeGUIService, resolver *fakeGUIResolver) *GUIHandler {
	t.Helper()
	handler, err := NewGUIHandler(service, resolver, NewGUIAccessStore())
	if err != nil {
		t.Fatalf("NewGUIHandler returned error: %v", err)
	}
	return handler
}

func authenticatedGUIRequest(t *testing.T, method, target string, userID, sessionID uuid.UUID) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	return request.WithContext(context.WithValue(request.Context(), authenticatedUserIDContextKey{}, userID))
}

func TestGUITicketRequiresAuthenticationOwnershipAndReadiness(t *testing.T) {
	userID, sessionID := uuid.New(), uuid.New()
	service := &fakeGUIService{reference: RuntimeReference{Namespace: defaultNamespace, PodName: "pod", ServiceName: "service"}}
	resolver := &fakeGUIResolver{}
	handler := newGUIHandlerForTest(t, service, resolver)
	mux := http.NewServeMux()
	labHandler, err := NewLabSessionHandler(service)
	if err != nil {
		t.Fatalf("NewLabSessionHandler returned error: %v", err)
	}
	if err := handler.RegisterRoute(labHandler); err != nil {
		t.Fatalf("RegisterRoute returned error: %v", err)
	}
	jwtService := newTestJWTService(t)
	if err := RegisterLabSessionRoutes(mux, labHandler, jwtService); err != nil {
		t.Fatalf("RegisterLabSessionRoutes returned error: %v", err)
	}

	unauthorized := httptest.NewRecorder()
	mux.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/api/labs/sessions/"+sessionID.String()+"/gui-ticket", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth status = %d, want 401", unauthorized.Code)
	}

	service.err = ErrLabSessionNotFound
	request := authenticatedGUIRequest(t, http.MethodPost, "/api/labs/sessions/"+sessionID.String()+"/gui-ticket", userID, sessionID)
	token, err := jwtService.IssueAccessToken(userID)
	if err != nil {
		t.Fatalf("IssueAccessToken returned error: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("cross-owner status = %d, want 404", response.Code)
	}

	service.err = ErrSessionNotReady
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("not-ready status = %d, want 409", response.Code)
	}
}

func TestGUIProxyUsesAuthorizedTargetAndSingleUseTicket(t *testing.T) {
	userID, sessionID := uuid.New(), uuid.New()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxy/vnc_auto.html" {
			t.Errorf("upstream path = %q, want /proxy/vnc_auto.html", r.URL.Path)
		}
		if r.URL.RawQuery != "keep=yes" {
			t.Errorf("upstream query = %q, want ticket removed", r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("backend credentials were forwarded upstream")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "kali-gui")
	}))
	defer upstream.Close()

	resolver := &fakeGUIResolver{target: guiProxyTarget{URL: mustParseURL(t, upstream.URL+"/proxy"), Transport: http.DefaultTransport}}
	service := &fakeGUIService{reference: RuntimeReference{Namespace: defaultNamespace, PodName: "pod", ServiceName: "service"}}
	handler := newGUIHandlerForTest(t, service, resolver)
	ticket, _, err := handler.access.issueTicket(userID, sessionID)
	if err != nil {
		t.Fatalf("issueTicket returned error: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/labs/sessions/"+sessionID.String()+"/gui/vnc_auto.html?ticket="+ticket+"&keep=yes", nil)
	request.Header.Set("Authorization", "Bearer should-not-forward")
	request.AddCookie(&http.Cookie{Name: "untrusted", Value: "value"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request, sessionID.String())
	if response.Code != http.StatusOK || response.Body.String() != "kali-gui" {
		t.Fatalf("proxy response = %d %q", response.Code, response.Body.String())
	}
	setCookie := response.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, guiAccessCookieName+"=") {
		t.Fatalf("GUI access cookie was not set: %q", setCookie)
	}

	second := httptest.NewRecorder()
	secondRequest := httptest.NewRequest(http.MethodGet, "/api/labs/sessions/"+sessionID.String()+"/gui/vnc_auto.html?keep=yes", nil)
	secondRequest.Header.Set("Cookie", strings.Split(setCookie, ";")[0])
	handler.ServeHTTP(second, secondRequest, sessionID.String())
	if second.Code != http.StatusOK {
		t.Fatalf("cookie reuse status = %d, want 200", second.Code)
	}

	replay := httptest.NewRecorder()
	handler.ServeHTTP(replay, request, sessionID.String())
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("ticket replay status = %d, want 401", replay.Code)
	}
}

func TestGUIProxyRejectsUnsafePathAndUpstreamFailure(t *testing.T) {
	userID, sessionID := uuid.New(), uuid.New()
	resolver := &fakeGUIResolver{target: guiProxyTarget{URL: mustParseURL(t, "http://127.0.0.1:1"), Transport: http.DefaultTransport}}
	service := &fakeGUIService{reference: RuntimeReference{Namespace: defaultNamespace, PodName: "pod", ServiceName: "service"}}
	handler := newGUIHandlerForTest(t, service, resolver)
	access, _, err := handler.access.issueAccess(userID, sessionID)
	if err != nil {
		t.Fatalf("issueAccess returned error: %v", err)
	}

	unsafeRequest := httptest.NewRequest(http.MethodGet, "/api/labs/sessions/"+sessionID.String()+"/gui/%2e%2e/private", nil)
	unsafeRequest.AddCookie(&http.Cookie{Name: guiAccessCookieName, Value: access})
	unsafeResponse := httptest.NewRecorder()
	handler.ServeHTTP(unsafeResponse, unsafeRequest, sessionID.String())
	if unsafeResponse.Code != http.StatusBadRequest {
		t.Fatalf("unsafe path status = %d, want 400", unsafeResponse.Code)
	}

	failureRequest := httptest.NewRequest(http.MethodGet, "/api/labs/sessions/"+sessionID.String()+"/gui/vnc_auto.html", nil)
	failureRequest.AddCookie(&http.Cookie{Name: guiAccessCookieName, Value: access})
	failureResponse := httptest.NewRecorder()
	handler.ServeHTTP(failureResponse, failureRequest, sessionID.String())
	if failureResponse.Code != http.StatusBadGateway {
		t.Fatalf("upstream failure status = %d, want 502", failureResponse.Code)
	}
}

type canceledGUITransport struct{}

func (canceledGUITransport) RoundTrip(request *http.Request) (*http.Response, error) {
	<-request.Context().Done()
	return nil, request.Context().Err()
}

func TestGUIProxyHandlesCanceledUpstreamAndSessionStop(t *testing.T) {
	userID, sessionID := uuid.New(), uuid.New()
	service := &fakeGUIService{reference: RuntimeReference{Namespace: defaultNamespace, PodName: "pod", ServiceName: "service"}}
	resolver := &fakeGUIResolver{target: guiProxyTarget{URL: mustParseURL(t, "http://upstream.invalid"), Transport: canceledGUITransport{}}}
	handler := newGUIHandlerForTest(t, service, resolver)
	access, _, err := handler.access.issueAccess(userID, sessionID)
	if err != nil {
		t.Fatalf("issueAccess returned error: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/labs/sessions/"+sessionID.String()+"/gui/vnc_auto.html", nil)
	requestContext, cancel := context.WithTimeout(request.Context(), 20*time.Millisecond)
	defer cancel()
	request = request.WithContext(requestContext)
	request.AddCookie(&http.Cookie{Name: guiAccessCookieName, Value: access})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request, sessionID.String())
	if response.Code != http.StatusBadGateway {
		t.Fatalf("canceled upstream status = %d, want 502", response.Code)
	}

	service.err = ErrSessionNotReady
	stoppedResponse := httptest.NewRecorder()
	stoppedRequest := httptest.NewRequest(http.MethodGet, "/api/labs/sessions/"+sessionID.String()+"/gui/vnc_auto.html", nil)
	stoppedRequest.AddCookie(&http.Cookie{Name: guiAccessCookieName, Value: access})
	handler.ServeHTTP(stoppedResponse, stoppedRequest, sessionID.String())
	if stoppedResponse.Code != http.StatusConflict {
		t.Fatalf("stopped session status = %d, want 409", stoppedResponse.Code)
	}
}

func TestGUIProxyWebSocketUpgrade(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		_ = connection.WriteMessage(websocket.TextMessage, []byte("gui-websocket"))
	}))
	defer upstream.Close()
	userID, sessionID := uuid.New(), uuid.New()
	service := &fakeGUIService{reference: RuntimeReference{Namespace: defaultNamespace, PodName: "pod", ServiceName: "service"}}
	resolver := &fakeGUIResolver{target: guiProxyTarget{URL: mustParseURL(t, upstream.URL), Transport: http.DefaultTransport}}
	handler := newGUIHandlerForTest(t, service, resolver)
	access, _, err := handler.access.issueAccess(userID, sessionID)
	if err != nil {
		t.Fatalf("issueAccess returned error: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r, sessionID.String())
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/labs/sessions/" + sessionID.String() + "/gui/websockify"
	connection, response, err := (&websocket.Dialer{}).Dial(wsURL, http.Header{"Cookie": []string{guiAccessCookieName + "=" + access}})
	if err != nil {
		t.Fatalf("WebSocket dial failed: %v response=%v", err, response)
	}
	defer connection.Close()
	_, message, err := connection.ReadMessage()
	if err != nil || string(message) != "gui-websocket" {
		t.Fatalf("WebSocket message = %q, %v", message, err)
	}
}

func mustParseURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	return parsed
}
