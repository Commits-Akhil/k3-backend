package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	terminalPathPrefix      = "/api/terminal"
	terminalIdleTimeout     = 2 * time.Minute
	terminalWriteTimeout    = 10 * time.Second
	terminalMaxMessageBytes = 64 * 1024
	terminalMaxColumns      = 500
	terminalMaxRows         = 200
)

type terminalSessionAuthorizer interface {
	GetTerminalReference(context.Context, uuid.UUID, uuid.UUID) (RuntimeReference, error)
}

type terminalStreamRuntime interface {
	StreamTerminal(context.Context, RuntimeReference, io.Reader, io.Writer, <-chan TerminalSize) error
}

type TerminalHandler struct {
	sessions terminalSessionAuthorizer
	runtime  terminalStreamRuntime
	tickets  *TerminalTicketStore
	origins  map[string]struct{}
	upgrade  websocket.Upgrader
}

type terminalTicketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
}

type terminalMessage struct {
	Type string `json:"type"`
	Data string `json:"data"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func NewTerminalHandler(sessions terminalSessionAuthorizer, runtime terminalStreamRuntime, tickets *TerminalTicketStore, origins []string) (*TerminalHandler, error) {
	if sessions == nil || runtime == nil || tickets == nil {
		return nil, errors.New("terminal dependencies are required")
	}
	originSet := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		origin = strings.TrimSpace(origin)
		if origin != "" {
			originSet[origin] = struct{}{}
		}
	}
	if len(originSet) == 0 {
		return nil, errors.New("terminal origin allowlist is required")
	}
	handler := &TerminalHandler{
		sessions: sessions,
		runtime:  runtime,
		tickets:  tickets,
		origins:  originSet,
	}
	handler.upgrade = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     handler.allowedOrigin,
	}
	return handler, nil
}

func (handler *TerminalHandler) RegisterTicketRoute(mux *http.ServeMux, labSessions *LabSessionHandler) error {
	if mux == nil || labSessions == nil {
		return errors.New("terminal route dependencies are required")
	}
	labSessions.terminalTicket = handler
	mux.Handle(terminalPathPrefix, http.HandlerFunc(handler.websocket))
	return nil
}

func (handler *TerminalHandler) IssueTicket(w http.ResponseWriter, r *http.Request, rawSessionID string) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	userID, err := AuthenticatedUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return
	}
	sessionID, err := uuid.Parse(rawSessionID)
	if err != nil || sessionID == uuid.Nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid request")
		return
	}
	if _, err := handler.sessions.GetTerminalReference(r.Context(), userID, sessionID); err != nil {
		if errors.Is(err, ErrLabSessionNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		if errors.Is(err, ErrSessionNotReady) {
			writeAPIError(w, http.StatusConflict, "not_ready", "lab session is not ready")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "server_error", "request could not be completed")
		return
	}
	ticket, expiresAt, err := handler.tickets.Issue(userID, sessionID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "request could not be completed")
		return
	}
	writeJSON(w, http.StatusOK, terminalTicketResponse{Ticket: ticket, ExpiresAt: expiresAt})
}

func (handler *TerminalHandler) websocket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || !handler.allowedOrigin(r) {
		writeAPIError(w, http.StatusForbidden, "forbidden", "forbidden")
		return
	}
	ticket := r.URL.Query().Get("ticket")
	userID, sessionID, err := handler.tickets.Consume(ticket)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return
	}
	reference, err := handler.sessions.GetTerminalReference(r.Context(), userID, sessionID)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return
	}
	conn, err := handler.upgrade.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(terminalMaxMessageBytes)
	streamContext, cancel := context.WithCancel(r.Context())
	defer cancel()
	input := newTerminalInput(streamContext)
	output := &websocketTerminalOutput{connection: conn}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		handler.readMessages(streamContext, conn, input, cancel)
	}()
	streamErr := handler.runtime.StreamTerminal(streamContext, reference, input, output, input.sizes)
	cancel()
	input.Close()
	_ = conn.Close()
	<-readDone
	_ = streamErr
}

func (handler *TerminalHandler) readMessages(ctx context.Context, conn *websocket.Conn, input *terminalInput, cancel context.CancelFunc) {
	_ = conn.SetReadDeadline(time.Now().Add(terminalIdleTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(terminalIdleTimeout))
	})
	for {
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			cancel()
			_ = conn.Close()
			return
		}
		if err := conn.SetReadDeadline(time.Now().Add(terminalIdleTimeout)); err != nil {
			cancel()
			return
		}
		if messageType != websocket.TextMessage {
			continue
		}
		var message terminalMessage
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&message); err != nil {
			cancel()
			return
		}
		switch message.Type {
		case "input":
			if len(message.Data) > terminalMaxMessageBytes || !input.Send([]byte(message.Data)) {
				cancel()
				return
			}
		case "resize":
			if message.Cols < 1 || message.Cols > terminalMaxColumns || message.Rows < 1 || message.Rows > terminalMaxRows || !input.Resize(TerminalSize{Cols: uint16(message.Cols), Rows: uint16(message.Rows)}) {
				cancel()
				return
			}
		default:
			cancel()
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func (handler *TerminalHandler) allowedOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	_, ok := handler.origins[origin]
	return ok
}

type websocketTerminalOutput struct {
	mu         sync.Mutex
	connection *websocket.Conn
}

func (output *websocketTerminalOutput) Write(data []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	if err := output.connection.SetWriteDeadline(time.Now().Add(terminalWriteTimeout)); err != nil {
		return 0, err
	}
	if err := output.connection.WriteMessage(websocket.BinaryMessage, data); err != nil {
		return 0, err
	}
	return len(data), nil
}

type terminalInput struct {
	context context.Context
	data    chan []byte
	sizes   chan TerminalSize
	mu      sync.Mutex
	closed  bool
	pending []byte
}

func newTerminalInput(ctx context.Context) *terminalInput {
	return &terminalInput{context: ctx, data: make(chan []byte), sizes: make(chan TerminalSize, 1)}
}

func (input *terminalInput) Read(buffer []byte) (int, error) {
	input.mu.Lock()
	if len(input.pending) > 0 {
		count := copy(buffer, input.pending)
		input.pending = input.pending[count:]
		input.mu.Unlock()
		return count, nil
	}
	input.mu.Unlock()

	select {
	case <-input.context.Done():
		return 0, input.context.Err()
	case data := <-input.data:
		count := copy(buffer, data)
		if count < len(data) {
			input.mu.Lock()
			input.pending = append(input.pending, data[count:]...)
			input.mu.Unlock()
		}
		return count, nil
	}
}

func (input *terminalInput) Send(data []byte) bool {
	input.mu.Lock()
	closed := input.closed
	input.mu.Unlock()
	if closed {
		return false
	}
	select {
	case input.data <- data:
		return true
	case <-input.context.Done():
		return false
	}
}

func (input *terminalInput) Resize(size TerminalSize) bool {
	input.mu.Lock()
	closed := input.closed
	input.mu.Unlock()
	if closed {
		return false
	}
	select {
	case input.sizes <- size:
		return true
	default:
		select {
		case <-input.sizes:
		default:
		}
		input.sizes <- size
		return true
	}
}

func (input *terminalInput) Close() {
	input.mu.Lock()
	input.closed = true
	input.mu.Unlock()
}
