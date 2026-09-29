package main

import "golang.org/x/crypto/bcrypt"

const (
	bcryptCost = 12
	// bcrypt only processes the first 72 password bytes; reject longer input.
	maxPasswordBytes = 72
)

func HashPassword(password string) (string, error) {
	if err := validatePassword(password); err != nil {
		return "", err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", ErrDatabaseOperation
	}
	return string(hash), nil
}

func VerifyPassword(password, passwordHash string) (bool, error) {
	if err := validatePassword(password); err != nil {
		return false, err
	}
	if passwordHash == "" {
		return false, ErrInvalidCredentials
	}

	err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password))
	if err == bcrypt.ErrMismatchedHashAndPassword || err == bcrypt.ErrHashTooShort {
		return false, nil
	}
	if err != nil {
		return false, ErrInvalidCredentials
	}
	return true, nil
}

func validatePassword(password string) error {
	if len(password) == 0 || len([]byte(password)) > maxPasswordBytes {
		return ErrInvalidInput
	}
	return nil
}
