package main

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

type RegisterStudentInput struct {
	Email    string
	Password string
}

type AuthenticateInput struct {
	Email    string
	Password string
}

type UserService struct {
	repository UserRepository
	now        func() time.Time
	newID      func() uuid.UUID
}

func NewUserService(repository UserRepository) (*UserService, error) {
	if repository == nil {
		return nil, ErrDatabaseOperation
	}
	return &UserService{
		repository: repository,
		now:        time.Now,
		newID:      uuid.New,
	}, nil
}

func (service *UserService) RegisterStudent(ctx context.Context, input RegisterStudentInput) (PublicUser, error) {
	email, err := NormalizeEmail(input.Email)
	if err != nil {
		return PublicUser{}, ErrInvalidInput
	}
	hash, err := HashPassword(input.Password)
	if err != nil {
		return PublicUser{}, err
	}

	now := service.now().UTC()
	user := User{
		ID:           service.newID(),
		Email:        email,
		PasswordHash: hash,
		Role:         UserRoleStudent,
		Status:       UserStatusActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := service.repository.Create(ctx, user); err != nil {
		return PublicUser{}, err
	}
	return user.Public(), nil
}

func (service *UserService) Authenticate(ctx context.Context, input AuthenticateInput) (PublicUser, error) {
	email, err := NormalizeEmail(input.Email)
	if err != nil || validatePassword(input.Password) != nil {
		return PublicUser{}, ErrInvalidCredentials
	}

	user, err := service.repository.FindByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return PublicUser{}, ErrInvalidCredentials
		}
		return PublicUser{}, err
	}
	if user.Status != UserStatusActive {
		return PublicUser{}, ErrAccountDisabled
	}

	valid, err := VerifyPassword(input.Password, user.PasswordHash)
	if err != nil || !valid {
		return PublicUser{}, ErrInvalidCredentials
	}

	lastLoginAt := service.now().UTC()
	if err := service.repository.UpdateLastLogin(ctx, user.ID, lastLoginAt); err != nil {
		return PublicUser{}, err
	}
	user.LastLoginAt = &lastLoginAt
	user.UpdatedAt = lastLoginAt
	return user.Public(), nil
}

func (service *UserService) GetByID(ctx context.Context, id uuid.UUID) (PublicUser, error) {
	if id == uuid.Nil {
		return PublicUser{}, ErrInvalidInput
	}
	user, err := service.repository.FindByID(ctx, id)
	if err != nil {
		return PublicUser{}, err
	}
	return user.Public(), nil
}
