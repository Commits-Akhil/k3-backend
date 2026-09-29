package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeUserRepository struct {
	mu             sync.Mutex
	users          map[string]User
	findByEmailErr error
	findByIDErr    error
	createErr      error
	lastLoginErr   error
	created        User
	lastLoginID    uuid.UUID
}

func (repository *fakeUserRepository) Create(_ context.Context, user User) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.createErr != nil {
		return repository.createErr
	}
	if repository.users == nil {
		repository.users = make(map[string]User)
	}
	if _, exists := repository.users[user.Email]; exists {
		return ErrDuplicateEmail
	}
	repository.users[user.Email] = user
	repository.created = user
	return nil
}

func (repository *fakeUserRepository) FindByEmail(_ context.Context, email string) (User, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.findByEmailErr != nil {
		return User{}, repository.findByEmailErr
	}
	user, ok := repository.users[email]
	if !ok {
		return User{}, ErrUserNotFound
	}
	return user, nil
}

func (repository *fakeUserRepository) FindByID(_ context.Context, id uuid.UUID) (User, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.findByIDErr != nil {
		return User{}, repository.findByIDErr
	}
	for _, user := range repository.users {
		if user.ID == id {
			return user, nil
		}
	}
	return User{}, ErrUserNotFound
}

func (repository *fakeUserRepository) UpdateLastLogin(_ context.Context, id uuid.UUID, lastLoginAt time.Time) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.lastLoginErr != nil {
		return repository.lastLoginErr
	}
	repository.lastLoginID = id
	for email, user := range repository.users {
		if user.ID == id {
			user.LastLoginAt = &lastLoginAt
			user.UpdatedAt = lastLoginAt
			repository.users[email] = user
			return nil
		}
	}
	return ErrUserNotFound
}

func newTestUserService(repository *fakeUserRepository) *UserService {
	service, err := NewUserService(repository)
	if err != nil {
		panic(err)
	}
	service.newID = func() uuid.UUID {
		return uuid.MustParse("22222222-2222-2222-2222-222222222222")
	}
	service.now = func() time.Time {
		return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	}
	return service
}

func TestUserServiceRegistersStudentWithNormalizedEmailAndHash(t *testing.T) {
	repository := &fakeUserRepository{}
	service := newTestUserService(repository)

	user, err := service.RegisterStudent(context.Background(), RegisterStudentInput{
		Email:    " Student@Example.COM ",
		Password: "test-password-for-unit-tests",
	})
	if err != nil {
		t.Fatalf("RegisterStudent returned error: %v", err)
	}
	if user.Role != UserRoleStudent || user.Status != UserStatusActive {
		t.Fatalf("registered account = %#v", user)
	}
	if user.Email != "student@example.com" {
		t.Fatalf("registered email = %q, want normalized email", user.Email)
	}
	repository.mu.Lock()
	created := repository.created
	repository.mu.Unlock()
	if created.PasswordHash == "" || created.PasswordHash == "test-password-for-unit-tests" {
		t.Fatal("repository received an unhashed password")
	}
	if publicUserHasPasswordHash(user) {
		t.Fatal("registration response exposes a password hash")
	}
}

func TestUserServiceRegistrationCannotCreateAdminAndMapsDuplicate(t *testing.T) {
	repository := &fakeUserRepository{}
	service := newTestUserService(repository)
	first, err := service.RegisterStudent(context.Background(), RegisterStudentInput{
		Email:    "student@example.com",
		Password: "test-password-for-unit-tests",
	})
	if err != nil {
		t.Fatalf("first RegisterStudent returned error: %v", err)
	}
	if first.Role == UserRoleAdmin {
		t.Fatal("student registration created an admin")
	}
	_, err = service.RegisterStudent(context.Background(), RegisterStudentInput{
		Email:    "STUDENT@example.com",
		Password: "another-test-password",
	})
	if !errors.Is(err, ErrDuplicateEmail) {
		t.Fatalf("duplicate registration error = %v, want ErrDuplicateEmail", err)
	}
}

func TestUserServiceAuthenticationUsesGenericCredentialFailure(t *testing.T) {
	repository := &fakeUserRepository{}
	service := newTestUserService(repository)
	if _, err := service.RegisterStudent(context.Background(), RegisterStudentInput{
		Email:    "student@example.com",
		Password: "test-password-for-unit-tests",
	}); err != nil {
		t.Fatalf("RegisterStudent returned error: %v", err)
	}

	success, err := service.Authenticate(context.Background(), AuthenticateInput{
		Email:    "STUDENT@example.com",
		Password: "test-password-for-unit-tests",
	})
	if err != nil || success.Email != "student@example.com" {
		t.Fatalf("successful Authenticate = %#v, %v", success, err)
	}

	_, wrongPasswordErr := service.Authenticate(context.Background(), AuthenticateInput{
		Email:    "student@example.com",
		Password: "wrong-password",
	})
	_, unknownEmailErr := service.Authenticate(context.Background(), AuthenticateInput{
		Email:    "unknown@example.com",
		Password: "wrong-password",
	})
	if !errors.Is(wrongPasswordErr, ErrInvalidCredentials) || !errors.Is(unknownEmailErr, ErrInvalidCredentials) || wrongPasswordErr.Error() != unknownEmailErr.Error() {
		t.Fatalf("credential errors differ: wrong=%v unknown=%v", wrongPasswordErr, unknownEmailErr)
	}
	if strings.Contains(wrongPasswordErr.Error(), repository.created.PasswordHash) {
		t.Fatal("password hash appeared in authentication error")
	}
}

func TestUserServiceRejectsDisabledAccountAndPropagatesRepositoryFailure(t *testing.T) {
	repository := &fakeUserRepository{}
	service := newTestUserService(repository)
	user := testUser()
	user.Status = UserStatusDisabled
	repository.users = map[string]User{user.Email: user}

	_, err := service.Authenticate(context.Background(), AuthenticateInput{Email: user.Email, Password: "test-password-for-unit-tests"})
	if !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("disabled account error = %v, want ErrAccountDisabled", err)
	}

	repository.findByEmailErr = ErrDatabaseOperation
	_, err = service.Authenticate(context.Background(), AuthenticateInput{Email: user.Email, Password: "test-password-for-unit-tests"})
	if !errors.Is(err, ErrDatabaseOperation) {
		t.Fatalf("repository failure = %v, want ErrDatabaseOperation", err)
	}
}

func TestUserServiceGetsUserByID(t *testing.T) {
	repository := &fakeUserRepository{users: map[string]User{}}
	user := testUser()
	repository.users[user.Email] = user
	service := newTestUserService(repository)

	public, err := service.GetByID(context.Background(), user.ID)
	if err != nil || public.ID != user.ID || public.Email != user.Email {
		t.Fatalf("GetByID = %#v, %v", public, err)
	}
	if _, err := service.GetByID(context.Background(), uuid.Nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nil UUID error = %v, want ErrInvalidInput", err)
	}
}
