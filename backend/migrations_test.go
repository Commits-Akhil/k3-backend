package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStage3AMigrationsHaveExpectedStructure(t *testing.T) {
	migrations := map[string][]string{
		"001_create_users.sql": {
			"-- +goose Up",
			"CREATE TABLE users",
			"CREATE UNIQUE INDEX users_email_unique_idx",
			"-- +goose Down",
		},
		"002_create_lab_sessions.sql": {
			"-- +goose Up",
			"CREATE TABLE lab_sessions",
			"REFERENCES users (id)",
			"'REQUESTED'",
			"'FAILED'",
			"CREATE INDEX lab_sessions_user_created_idx",
			"-- +goose Down",
		},
	}

	for filename, requiredParts := range migrations {
		contents, err := os.ReadFile(filepath.Join("..", "migrations", filename))
		if err != nil {
			t.Fatalf("read migration %s: %v", filename, err)
		}
		text := string(contents)
		for _, requiredPart := range requiredParts {
			if !strings.Contains(text, requiredPart) {
				t.Errorf("migration %s does not contain %q", filename, requiredPart)
			}
		}
	}
}
