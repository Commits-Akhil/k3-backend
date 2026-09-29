package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type fakeTerminalAuthorizer struct {
	mu        sync.Mutex
	reference RuntimeReference
	err       error
	userID    uuid.UUID
	sessionID uuid.UUID
}

func (authorizer *fakeTerminalAuthorizer) GetTerminalReference(_ context.Context, userID, sessionID uuid.UUID) (RuntimeReference, error) {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	authorizer.userID, authorizer.sessionID = userID, sessionID
	if authorizer.err != nil {
		return RuntimeReference{}, authorizer.err
	}
	return authorizer.reference, nil
}

type fakeTerminalRuntime struct {
	mu             sync.Mutex
	streamCalls    int
	streamStarted  chan struct{}
	streamCanceled chan struct{}
	writeOutput    bool
	waitForCancel  bool
}

func (runtime *fakeTerminalRuntime) StreamTerminal(ctx context.Context, _ RuntimeReference, input io.Reader, output io.Writer, _ <-chan TerminalSize) error {
	runtime.mu.Lock()
	runtime.streamCalls++
	if runtime.streamStarted != nil {
		close(runtime.streamStarted)
	}
	writeOutput := runtime.writeOutput
	waitForCancel := runtime.waitForCancel
	runtime.mu.Unlock()
	if writeOutput {
		_, _ = output.Write([]byte("terminal-output"))
	}
	if !waitForCancel {
		return nil
	}
	<-ctx.Done()
	if runtime.streamCanceled != nil {
		close(runtime.streamCanceled)
	}
	return ctx.Err()
}

func newTestTerminalHandler(t *testing.T, authorizer *fakeTerminalAuthorizer, runtime *fakeTerminalRuntime) (*TerminalHandler, *JWTService) {
	t.Helper()
	handler, err := NewTerminalHandler(
		authorizer,
		runtime,
		NewTerminalTicketStore(),
		[]string{"http://allowed.example"},
	)
	if err != nil {
		t.Fatalf("NewTerminalHandler returned error: %v", err)
	}
	return handler, newTestJWTService(t)
}

func authenticatedContextRequest(t *testing.T, method, target string, userID uuid.UUID) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	return request.WithContext(context.WithValue(request.Context(), authenticatedUserIDContextKey{}, userID))
}

func TestTerminalTicketStoreExpiresAndConsumesOnce(t *testing.T) {
	store := NewTerminalTicketStore()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	userID, sessionID := uuid.New(), uuid.New()
	ticket, _, err := store.Issue(userID, sessionID)
	if err != nil {
		t.Fatalf("Issue returned error: %v", err)
	}
	gotUser, gotSession, err := store.Consume(ticket)
	if err != nil || gotUser != userID || gotSession != sessionID {
		t.Fatalf("Consume = %s, %s, %v", gotUser, gotSession, err)
	}
	if _, _, err := store.Consume(ticket); !errors.Is(err, ErrInvalidTerminalTicket) {
		t.Fatalf("reused ticket error = %v, want ErrInvalidTerminalTicket", err)
	}
	ticket, _, err = store.Issue(userID, sessionID)
	if err != nil {
		t.Fatalf("second Issue returned error: %v", err)
	}
	now = now.Add(terminalTicketTTL)
	if _, _, err := store.Consume(ticket); !errors.Is(err, ErrInvalidTerminalTicket) {
		t.Fatalf("expired ticket error = %v, want ErrInvalidTerminalTicket", err)
	}
}

func TestTerminalTicketStoreConcurrentConsumptionAllowsOneUse(t *testing.T) {
	store := NewTerminalTicketStore()
	ticket, _, err := store.Issue(uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("Issue returned error: %v", err)
	}
	var waitGroup sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for index := 0; index < 16; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			if _, _, consumeErr := store.Consume(ticket); consumeErr == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	waitGroup.Wait()
	if successes != 1 {
		t.Fatalf("successful ticket consumptions = %d, want 1", successes)
	}
}

func TestTerminalTicketRouteRequiresAuthenticationAndReadyOwnership(t *testing.T) {
	userID := uuid.New()
	sessionID := uuid.New()
	authorizer := &fakeTerminalAuthorizer{reference: RuntimeReference{Namespace: defaultNamespace, PodName: "pod", ServiceName: "service"}}
	runtime := &fakeTerminalRuntime{}
	handler, jwtService := newTestTerminalHandler(t, authorizer, runtime)
	service := &fakeLabSessionService{
		createFn: func(context.Context, uuid.UUID, string) (LabSessionView, error) { return LabSessionView{}, nil },
		getFn:    func(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error) { return LabSessionView{}, nil },
		listFn:   func(context.Context, uuid.UUID, int, int) ([]LabSessionView, error) { return nil, nil },
		stopFn:   func(context.Context, uuid.UUID, uuid.UUID) (LabSessionView, error) { return LabSessionView{}, nil },
	}
	labHandler, err := NewLabSessionHandler(service)
	if err != nil {
		t.Fatalf("NewLabSessionHandler returned error: %v", err)
	}
	mux := http.NewServeMux()
	if err := RegisterLabSessionRoutes(mux, labHandler, jwtService); err != nil {
		t.Fatalf("RegisterLabSessionRoutes returned error: %v", err)
	}
	if err := handler.RegisterTicketRoute(mux, labHandler); err != nil {
		t.Fatalf("RegisterTicketRoute returned error: %v", err)
	}
	unauthenticated := httptest.NewRecorder()
	mux.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodPost, "/api/labs/sessions/"+sessionID.String()+"/terminal-ticket", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated ticket status = %d, want 401", unauthenticated.Code)
	}

	request := authenticatedContextRequest(t, http.MethodPost, "/api/labs/sessions/"+sessionID.String()+"/terminal-ticket", userID)
	token, err := jwtService.IssueAccessToken(userID)
	if err != nil {
		t.Fatalf("IssueAccessToken returned error: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "ticket") {
		t.Fatalf("ticket response = %d %s", response.Code, response.Body.String())
	}
	authorizer.err = ErrSessionNotReady
	request = authenticatedContextRequest(t, http.MethodPost, "/api/labs/sessions/"+sessionID.String()+"/terminal-ticket", uuid.New())
	request.Header.Set("Authorization", "Bearer "+token)
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("not-ready ticket status = %d, want 409", response.Code)
	}
}

func TestTerminalWebSocketValidTicketBindsRuntimeAndOrigin(t *testing.T) {
	userID, sessionID := uuid.New(), uuid.New()
	authorizer := &fakeTerminalAuthorizer{reference: RuntimeReference{Namespace: defaultNamespace, PodName: "pod", ServiceName: "service"}}
	runtime := &fakeTerminalRuntime{writeOutput: true}
	handler, _ := newTestTerminalHandler(t, authorizer, runtime)
	ticket, _, err := handler.tickets.Issue(userID, sessionID)
	if err != nil {
		t.Fatalf("Issue returned error: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(handler.websocket))
	defer server.Close()
	websocketURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/terminal?ticket=" + ticket
	dialer := websocket.Dialer{}
	connection, response, err := dialer.Dial(websocketURL, http.Header{"Origin": []string{"http://allowed.example"}})
	if err != nil {
		t.Fatalf("Dial returned error: %v, response=%v", err, response)
	}
	_, output, err := connection.ReadMessage()
	if err != nil || string(output) != "terminal-output" {
		t.Fatalf("terminal output = %q, %v", output, err)
	}
	_ = connection.Close()
	authorizer.mu.Lock()
	boundUser, boundSession := authorizer.userID, authorizer.sessionID
	authorizer.mu.Unlock()
	if boundUser != userID || boundSession != sessionID {
		t.Fatalf("runtime authorization binding = %s, %s", boundUser, boundSession)
	}
}

func TestTerminalWebSocketRejectsOriginAndInvalidTicketBeforeUpgrade(t *testing.T) {
	authorizer := &fakeTerminalAuthorizer{}
	runtime := &fakeTerminalRuntime{}
	handler, _ := newTestTerminalHandler(t, authorizer, runtime)
	server := httptest.NewServer(http.HandlerFunc(handler.websocket))
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/terminal?ticket=invalid"
	for name, headers := range map[string]http.Header{
		"missing origin":    {},
		"disallowed origin": {"Origin": []string{"http://blocked.example"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, response, err := dialTerminalTestWebSocket(url, headers)
			if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
				t.Fatalf("Dial error=%v response=%v, want 403", err, response)
			}
		})
	}
}

func TestTerminalWebSocketRejectsRuntimeAuthorizationFailure(t *testing.T) {
	userID, sessionID := uuid.New(), uuid.New()
	authorizer := &fakeTerminalAuthorizer{err: ErrSessionOperation}
	runtime := &fakeTerminalRuntime{}
	handler, _ := newTestTerminalHandler(t, authorizer, runtime)
	ticket, _, err := handler.tickets.Issue(userID, sessionID)
	if err != nil {
		t.Fatalf("Issue returned error: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(handler.websocket))
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/terminal?ticket=" + ticket
	_, response, err := dialTerminalTestWebSocket(url, http.Header{"Origin": []string{"http://allowed.example"}})
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized || runtime.streamCalls != 0 {
		t.Fatalf("Dial error=%v response=%v stream calls=%d", err, response, runtime.streamCalls)
	}
}

func TestTerminalWebSocketDisconnectCancelsRuntimeStream(t *testing.T) {
	userID, sessionID := uuid.New(), uuid.New()
	authorizer := &fakeTerminalAuthorizer{reference: RuntimeReference{Namespace: defaultNamespace, PodName: "pod", ServiceName: "service"}}
	runtime := &fakeTerminalRuntime{waitForCancel: true, streamStarted: make(chan struct{}), streamCanceled: make(chan struct{})}
	handler, _ := newTestTerminalHandler(t, authorizer, runtime)
	ticket, _, err := handler.tickets.Issue(userID, sessionID)
	if err != nil {
		t.Fatalf("Issue returned error: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(handler.websocket))
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/terminal?ticket=" + ticket
	connection, _, err := dialTerminalTestWebSocket(url, http.Header{"Origin": []string{"http://allowed.example"}})
	if err != nil {
		t.Fatalf("Dial returned error: %v", err)
	}
	<-runtime.streamStarted
	_ = connection.Close()
	select {
	case <-runtime.streamCanceled:
	case <-time.After(time.Second):
		t.Fatal("runtime stream was not canceled after WebSocket disconnect")
	}
}

func TestTerminalWebSocketMalformedAndOversizedMessagesCloseStream(t *testing.T) {
	for name, payload := range map[string]string{
		"malformed":   "{",
		"unsupported": `{"type":"unknown"}`,
		"oversized":   `{"type":"input","data":"` + strings.Repeat("x", terminalMaxMessageBytes) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			userID, sessionID := uuid.New(), uuid.New()
			authorizer := &fakeTerminalAuthorizer{reference: RuntimeReference{Namespace: defaultNamespace, PodName: "pod", ServiceName: "service"}}
			runtime := &fakeTerminalRuntime{waitForCancel: true}
			handler, _ := newTestTerminalHandler(t, authorizer, runtime)
			ticket, _, err := handler.tickets.Issue(userID, sessionID)
			if err != nil {
				t.Fatalf("Issue returned error: %v", err)
			}
			server := httptest.NewServer(http.HandlerFunc(handler.websocket))
			defer server.Close()
			url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/terminal?ticket=" + ticket
			connection, _, err := dialTerminalTestWebSocket(url, http.Header{"Origin": []string{"http://allowed.example"}})
			if err != nil {
				t.Fatalf("Dial returned error: %v", err)
			}
			if err := connection.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
				t.Fatalf("WriteMessage returned error: %v", err)
			}
			_ = connection.Close()
		})
	}
}

func dialTerminalTestWebSocket(url string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	dialer := websocket.Dialer{}
	return dialer.Dial(url, headers)
}
