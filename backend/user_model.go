package main

import (
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
)

type UserRole string

const (
	UserRoleStudent UserRole = "student"
	UserRoleAdmin   UserRole = "admin"
)

type UserStatus string

const (
	UserStatusActive   UserStatus = "active"
	UserStatusDisabled UserStatus = "disabled"
)

type User struct {
	ID           uuid.UUID
	Email        string
	PasswordHash string
	Role         UserRole
	Status       UserStatus
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastLoginAt  *time.Time
}

type PublicUser struct {
	ID          uuid.UUID  `json:"id"`
	Email       string     `json:"email"`
	Role        UserRole   `json:"role"`
	Status      UserStatus `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

func (user User) Public() PublicUser {
	return PublicUser{
		ID:          user.ID,
		Email:       user.Email,
		Role:        user.Role,
		Status:      user.Status,
		CreatedAt:   user.CreatedAt,
		UpdatedAt:   user.UpdatedAt,
		LastLoginAt: user.LastLoginAt,
	}
}

func NormalizeEmail(email string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(email))
	if normalized == "" || len(normalized) > 254 {
		return "", ErrInvalidInput
	}

	parsed, err := mail.ParseAddress(normalized)
	if err != nil || parsed.Address != normalized {
		return "", ErrInvalidInput
	}
	return normalized, nil
}

func validateUser(user User) error {
	if user.ID == uuid.Nil || user.Email == "" || user.PasswordHash == "" {
		return ErrInvalidInput
	}
	normalizedEmail, err := NormalizeEmail(user.Email)
	if err != nil || normalizedEmail != user.Email {
		return ErrInvalidInput
	}
	if user.Role != UserRoleStudent && user.Role != UserRoleAdmin {
		return ErrInvalidInput
	}
	if user.Status != UserStatusActive && user.Status != UserStatusDisabled {
		return ErrInvalidInput
	}
	if user.CreatedAt.IsZero() || user.UpdatedAt.IsZero() {
		return ErrInvalidInput
	}
	return nil
}

var (
	ErrInvalidInput       = errors.New("invalid input")
	ErrDuplicateEmail     = errors.New("email already exists")
	ErrUserNotFound       = errors.New("user not found")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrAccountDisabled    = errors.New("account is disabled")
	ErrDatabaseOperation  = errors.New("database operation failed")
)
