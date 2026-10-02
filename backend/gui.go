package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	guiPathPrefix       = "/api/labs/sessions/"
	guiTicketTTL        = 30 * time.Second
	guiAccessTTL        = 10 * time.Minute
	guiAccessCookieName = "cyberlab_gui_access"
	guiMaxRequestBody   = 1 << 20
	guiHTTPTimeout      = 30 * time.Second
)

var ErrInvalidGUIAccess = errors.New("invalid GUI access")

type guiAccess struct {
	userID    uuid.UUID
	sessionID uuid.UUID
	expiresAt time.Time
}

type GUIAccessStore struct {
	mu      sync.Mutex
	entries map[string]guiAccess
	now     func() time.Time
}

func NewGUIAccessStore() *GUIAccessStore {
	return &GUIAccessStore{entries: make(map[string]guiAccess), now: time.Now}
}

func (store *GUIAccessStore) issue(userID, sessionID uuid.UUID, ttl time.Duration) (string, time.Time, error) {
	if store == nil || userID == uuid.Nil || sessionID == uuid.Nil || ttl <= 0 {
		return "", time.Time{}, ErrInvalidInput
	}
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", time.Time{}, ErrInvalidToken
	}
	token := base64.RawURLEncoding.EncodeToString(value)
	expiresAt := store.now().UTC().Add(ttl)
	store.mu.Lock()
	store.removeExpiredLocked(store.now().UTC())
	store.entries[token] = guiAccess{userID: userID, sessionID: sessionID, expiresAt: expiresAt}
	store.mu.Unlock()
	return token, expiresAt, nil
}

func (store *GUIAccessStore) issueTicket(userID, sessionID uuid.UUID) (string, time.Time, error) {
	return store.issue(userID, sessionID, guiTicketTTL)
}

func (store *GUIAccessStore) issueAccess(userID, sessionID uuid.UUID) (string, time.Time, error) {
	return store.issue(userID, sessionID, guiAccessTTL)
}

func (store *GUIAccessStore) consume(token string) (uuid.UUID, uuid.UUID, error) {
	if store == nil || token == "" {
		return uuid.Nil, uuid.Nil, ErrInvalidGUIAccess
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.entries[token]
	delete(store.entries, token)
	if !ok || !store.now().UTC().Before(entry.expiresAt) {
		return uuid.Nil, uuid.Nil, ErrInvalidGUIAccess
	}
	return entry.userID, entry.sessionID, nil
}

func (store *GUIAccessStore) validate(token string) (uuid.UUID, uuid.UUID, error) {
	if store == nil || token == "" {
		return uuid.Nil, uuid.Nil, ErrInvalidGUIAccess
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.entries[token]
	if !ok || !store.now().UTC().Before(entry.expiresAt) {
		delete(store.entries, token)
		return uuid.Nil, uuid.Nil, ErrInvalidGUIAccess
	}
	return entry.userID, entry.sessionID, nil
}

func (store *GUIAccessStore) removeExpiredLocked(now time.Time) {
	for token, entry := range store.entries {
		if !now.Before(entry.expiresAt) {
			delete(store.entries, token)
		}
	}
}

type guiTicketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
}

type guiProxyTarget struct {
	URL       *url.URL
	Transport http.RoundTripper
}

type guiProxyResolver interface {
	ResolveGUIProxyTarget(context.Context, RuntimeReference) (guiProxyTarget, error)
}

type guiSessionAuthorizer interface {
	GetTerminalReference(context.Context, uuid.UUID, uuid.UUID) (RuntimeReference, error)
}

type GUIHandler struct {
	sessions guiSessionAuthorizer
	resolver guiProxyResolver
	access   *GUIAccessStore
}

func NewGUIHandler(sessions guiSessionAuthorizer, resolver guiProxyResolver, access *GUIAccessStore) (*GUIHandler, error) {
	if sessions == nil || resolver == nil || access == nil {
		return nil, errors.New("GUI dependencies are required")
	}
	return &GUIHandler{sessions: sessions, resolver: resolver, access: access}, nil
}

func (handler *GUIHandler) RegisterRoute(labSessions *LabSessionHandler) error {
	if labSessions == nil {
		return errors.New("GUI route dependencies are required")
	}
	labSessions.gui = handler
	return nil
}

func (handler *GUIHandler) IssueTicket(w http.ResponseWriter, r *http.Request, rawSessionID string) {
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
		handler.writeAuthorizationError(w, err)
		return
	}
	ticket, expiresAt, err := handler.access.issueTicket(userID, sessionID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "request could not be completed")
		return
	}
	writeJSON(w, http.StatusOK, guiTicketResponse{Ticket: ticket, ExpiresAt: expiresAt})
}

func (handler *GUIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, rawSessionID string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeMethodNotAllowed(w, http.MethodGet+", "+http.MethodHead)
		return
	}
	sessionID, err := uuid.Parse(rawSessionID)
	if err != nil || sessionID == uuid.Nil || unsafeGUIPath(r.URL) {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid request")
		return
	}
	userID, ok := handler.authorizeBrowser(w, r, sessionID)
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return
	}
	reference, err := handler.sessions.GetTerminalReference(r.Context(), userID, sessionID)
	if err != nil {
		handler.writeAuthorizationError(w, err)
		return
	}
	target, err := handler.resolver.ResolveGUIProxyTarget(r.Context(), reference)
	if err != nil || target.URL == nil || target.Transport == nil {
		writeAPIError(w, http.StatusBadGateway, "upstream_unavailable", "GUI is unavailable")
		return
	}
	proxyPath, ok := guiProxyPath(r.URL.Path, sessionID)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid request")
		return
	}
	proxyURL := *target.URL
	proxyURL.Path = strings.TrimRight(proxyURL.Path, "/") + "/" + strings.TrimLeft(proxyPath, "/")
	proxyURL.RawPath = ""
	query := r.URL.Query()
	query.Del("ticket")
	proxyURL.RawQuery = query.Encode()
	proxy := &httputil.ReverseProxy{
		Transport: target.Transport,
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(&proxyURL)
			request.Out.URL.Path = proxyURL.Path
			request.Out.URL.RawPath = proxyURL.RawPath
			request.Out.URL.RawQuery = proxyURL.RawQuery
			request.Out.Host = proxyURL.Host
			request.Out.Header.Del("Authorization")
			request.Out.Header.Del("Cookie")
			request.Out.Header.Del("Forwarded")
			request.Out.Header.Del("X-Forwarded-For")
			request.Out.Header.Del("X-Forwarded-Host")
			request.Out.Header.Del("X-Forwarded-Proto")
		},
		ErrorHandler: func(http.ResponseWriter, *http.Request, error) {
			writeAPIError(w, http.StatusBadGateway, "upstream_unavailable", "GUI is unavailable")
		},
	}
	r.Body = http.MaxBytesReader(w, r.Body, guiMaxRequestBody)
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		requestContext, cancel := context.WithTimeout(r.Context(), guiHTTPTimeout)
		defer cancel()
		r = r.WithContext(requestContext)
	}
	proxy.ServeHTTP(w, r)
}

func (handler *GUIHandler) authorizeBrowser(w http.ResponseWriter, r *http.Request, sessionID uuid.UUID) (uuid.UUID, bool) {
	if ticket := r.URL.Query().Get("ticket"); ticket != "" {
		userID, ticketSessionID, err := handler.access.consume(ticket)
		if err != nil || ticketSessionID != sessionID {
			return uuid.Nil, false
		}
		accessToken, expiresAt, err := handler.access.issueAccess(userID, sessionID)
		if err != nil {
			return uuid.Nil, false
		}
		http.SetCookie(w, &http.Cookie{Name: guiAccessCookieName, Value: accessToken, Path: guiPathPrefix + sessionID.String() + "/gui/", Expires: expiresAt, MaxAge: int(guiAccessTTL.Seconds()), HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode})
		return userID, true
	}
	cookie, err := r.Cookie(guiAccessCookieName)
	if err != nil {
		return uuid.Nil, false
	}
	return handler.access.consumeAndRestore(cookie.Value, sessionID)
}

func (store *GUIAccessStore) consumeAndRestore(token string, sessionID uuid.UUID) (uuid.UUID, bool) {
	userID, storedSessionID, err := store.validate(token)
	if err != nil || storedSessionID != sessionID {
		return uuid.Nil, false
	}
	return userID, true
}

func (handler *GUIHandler) writeAuthorizationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrLabSessionNotFound):
		writeAPIError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, ErrSessionNotReady):
		writeAPIError(w, http.StatusConflict, "not_ready", "lab session is not ready")
	default:
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
	}
}

func guiProxyPath(requestPath string, sessionID uuid.UUID) (string, bool) {
	prefix := guiPathPrefix + sessionID.String() + "/gui"
	if requestPath == prefix {
		return "/", true
	}
	if !strings.HasPrefix(requestPath, prefix+"/") {
		return "", false
	}
	return strings.TrimPrefix(requestPath, prefix), true
}

func unsafeGUIPath(value *url.URL) bool {
	if value == nil {
		return true
	}
	decoded, err := url.PathUnescape(value.EscapedPath())
	if err != nil || strings.Contains(decoded, "\\") {
		return true
	}
	for _, part := range strings.Split(decoded, "/") {
		if part == "." || part == ".." {
			return true
		}
	}
	return false
}
