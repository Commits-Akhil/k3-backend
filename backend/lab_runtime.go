package main

import "context"

type SessionState string

const (
	SessionRequested SessionState = "REQUESTED"
	SessionStarting  SessionState = "STARTING"
	SessionReady     SessionState = "READY"
	SessionStopping  SessionState = "STOPPING"
	SessionStopped   SessionState = "STOPPED"
	SessionFailed    SessionState = "FAILED"
)

type RuntimePhase string

const (
	RuntimeStarting RuntimePhase = "STARTING"
	RuntimeReady    RuntimePhase = "READY"
	RuntimeStopped  RuntimePhase = "STOPPED"
	RuntimeMissing  RuntimePhase = "MISSING"
	RuntimeOrphaned RuntimePhase = "ORPHANED"
	RuntimeFailed   RuntimePhase = "FAILED"
)

type RuntimeCreateRequest struct {
	SessionID string
	LabID     string
}

type RuntimeReference struct {
	Namespace   string
	PodName     string
	ServiceName string
	Labels      map[string]string
}

type RuntimeStatus struct {
	Phase RuntimePhase
	Ready bool
}

type TerminalSize struct {
	Cols uint16
	Rows uint16
}

type LabRuntime interface {
	Create(ctx context.Context, request RuntimeCreateRequest) (RuntimeReference, error)
	Status(ctx context.Context, reference RuntimeReference) (RuntimeStatus, error)
	Stop(ctx context.Context, reference RuntimeReference) error
}
