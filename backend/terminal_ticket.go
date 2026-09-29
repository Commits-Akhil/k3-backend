package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

const terminalTicketTTL = 30 * time.Second

var ErrInvalidTerminalTicket = errors.New("invalid terminal ticket")

type terminalTicket struct {
	userID    uuid.UUID
	sessionID uuid.UUID
	expiresAt time.Time
}

type TerminalTicketStore struct {
	mu      sync.Mutex
	tickets map[string]terminalTicket
	now     func() time.Time
}

func NewTerminalTicketStore() *TerminalTicketStore {
	return &TerminalTicketStore{
		tickets: make(map[string]terminalTicket),
		now:     time.Now,
	}
}

func (store *TerminalTicketStore) Issue(userID, sessionID uuid.UUID) (string, time.Time, error) {
	if store == nil || userID == uuid.Nil || sessionID == uuid.Nil {
		return "", time.Time{}, ErrInvalidInput
	}
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", time.Time{}, ErrInvalidToken
	}
	ticket := base64.RawURLEncoding.EncodeToString(bytes)
	expiresAt := store.now().UTC().Add(terminalTicketTTL)
	store.mu.Lock()
	store.removeExpiredLocked(store.now().UTC())
	store.tickets[ticket] = terminalTicket{userID: userID, sessionID: sessionID, expiresAt: expiresAt}
	store.mu.Unlock()
	return ticket, expiresAt, nil
}

func (store *TerminalTicketStore) Consume(ticket string) (uuid.UUID, uuid.UUID, error) {
	if store == nil || ticket == "" {
		return uuid.Nil, uuid.Nil, ErrInvalidTerminalTicket
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.tickets[ticket]
	delete(store.tickets, ticket)
	if !ok || !store.now().UTC().Before(entry.expiresAt) {
		return uuid.Nil, uuid.Nil, ErrInvalidTerminalTicket
	}
	return entry.userID, entry.sessionID, nil
}

func (store *TerminalTicketStore) removeExpiredLocked(now time.Time) {
	for ticket, entry := range store.tickets {
		if !now.Before(entry.expiresAt) {
			delete(store.tickets, ticket)
		}
	}
}
