package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func validEnvironment() map[string]string {
	return map[string]string{
		databaseDSNEnv: "postgres://localhost/cyberlab",
		jwtSecretEnv:   strings.Repeat("s", minimumJWTSecretBytes),
	}
}

func envReader(values map[string]string) func(string) string {
	return func(key string) string {
		return values[key]
	}
}

func TestLoadConfigRejectsMissingDatabaseDSN(t *testing.T) {
	values := validEnvironment()
	delete(values, databaseDSNEnv)

	_, err := LoadConfigFromEnv(envReader(values))
	if err == nil || !strings.Contains(err.Error(), "connection string") {
		t.Fatalf("error = %v, want sanitized missing DSN error", err)
	}
}

func TestLoadConfigRejectsShortJWTSecretWithoutExposingIt(t *testing.T) {
	values := validEnvironment()
	secret := "too-short-secret"
	values[jwtSecretEnv] = secret

	_, err := LoadConfigFromEnv(envReader(values))
	if err == nil {
		t.Fatal("short JWT secret was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("JWT secret was exposed in the error")
	}
}

func TestLoadConfigParsesValidValuesAndDefaults(t *testing.T) {
	values := validEnvironment()
	config, err := LoadConfigFromEnv(envReader(values))
	if err != nil {
		t.Fatalf("LoadConfigFromEnv returned error: %v", err)
	}
	if config.Database.MaxOpenConns != defaultMaxOpenConns || config.Database.MaxIdleConns != defaultMaxIdleConns {
		t.Fatalf("unexpected pool defaults: %#v", config.Database)
	}
	if config.Database.ConnMaxLifetime != defaultConnMaxLifetime || config.Database.PingTimeout != defaultDatabasePingTimeout {
		t.Fatalf("unexpected duration defaults: %#v", config.Database)
	}

	values[maxOpenConnsEnv] = "20"
	values[maxIdleConnsEnv] = "10"
	values[connMaxLifetimeEnv] = "1h"
	values[connMaxIdleTimeEnv] = "10m"
	values[databasePingTimeoutEnv] = "2s"
	config, err = LoadConfigFromEnv(envReader(values))
	if err != nil {
		t.Fatalf("LoadConfigFromEnv with overrides returned error: %v", err)
	}
	if config.Database.MaxOpenConns != 20 || config.Database.MaxIdleConns != 10 || config.Database.ConnMaxLifetime != time.Hour || config.Database.ConnMaxIdleTime != 10*time.Minute || config.Database.PingTimeout != 2*time.Second {
		t.Fatalf("configuration overrides were not parsed: %#v", config.Database)
	}
}

func TestLoadConfigRejectsInvalidPoolSettings(t *testing.T) {
	values := validEnvironment()
	values[maxIdleConnsEnv] = "11"
	values[maxOpenConnsEnv] = "10"

	_, err := LoadConfigFromEnv(envReader(values))
	if err == nil {
		t.Fatal("invalid pool settings were accepted")
	}
}

func TestOpenDatabaseConfiguresPoolWithoutConnecting(t *testing.T) {
	db, err := OpenDatabase(DatabaseConfig{
		DSN:             "postgres://localhost/cyberlab",
		MaxOpenConns:    4,
		MaxIdleConns:    2,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: time.Second,
		PingTimeout:     time.Second,
	})
	if err != nil {
		t.Fatalf("OpenDatabase returned error: %v", err)
	}
	defer db.Close()

	stats := db.Stats()
	if stats.MaxOpenConnections != 4 {
		t.Fatalf("max open connections = %d, want 4", stats.MaxOpenConnections)
	}
}

func TestDatabaseErrorsDoNotExposeDSN(t *testing.T) {
	dsn := "postgres://[invalid"
	_, err := OpenDatabase(DatabaseConfig{DSN: dsn})
	if err == nil || strings.Contains(err.Error(), dsn) {
		t.Fatalf("error = %v, contains sensitive connection details", err)
	}

	if err := CheckDatabase(context.Background(), nil, time.Second); err == nil {
		t.Fatal("CheckDatabase accepted nil database")
	}
	if err := CheckDatabase(context.Background(), &sql.DB{}, 0); err == nil {
		t.Fatal("CheckDatabase accepted non-positive timeout")
	}
}
