package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	databaseDSNEnv         = "CYBERLAB_DATABASE_URL"
	jwtSecretEnv           = "CYBERLAB_JWT_SECRET"
	maxOpenConnsEnv        = "CYBERLAB_DB_MAX_OPEN_CONNS"
	maxIdleConnsEnv        = "CYBERLAB_DB_MAX_IDLE_CONNS"
	connMaxLifetimeEnv     = "CYBERLAB_DB_CONN_MAX_LIFETIME"
	connMaxIdleTimeEnv     = "CYBERLAB_DB_CONN_MAX_IDLE_TIME"
	databasePingTimeoutEnv = "CYBERLAB_DB_PING_TIMEOUT"
	frontendOriginsEnv     = "CYBERLAB_FRONTEND_ORIGINS"

	defaultMaxOpenConns        = 10
	defaultMaxIdleConns        = 5
	defaultConnMaxLifetime     = 30 * time.Minute
	defaultConnMaxIdleTime     = 5 * time.Minute
	defaultDatabasePingTimeout = 5 * time.Second
	minimumJWTSecretBytes      = 32
)

type AppConfig struct {
	Database        DatabaseConfig
	JWT             JWTConfig
	FrontendOrigins []string
}

type DatabaseConfig struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
	PingTimeout     time.Duration
}

type JWTConfig struct {
	SigningSecret string
}

func LoadConfig() (AppConfig, error) {
	return LoadConfigFromEnv(func(key string) string {
		return os.Getenv(key)
	})
}

func LoadConfigFromEnv(getenv func(string) string) (AppConfig, error) {
	if getenv == nil {
		return AppConfig{}, errors.New("configuration environment reader is required")
	}

	database, err := loadDatabaseConfig(getenv)
	if err != nil {
		return AppConfig{}, err
	}

	secret := getenv(jwtSecretEnv)
	if len(secret) < minimumJWTSecretBytes {
		return AppConfig{}, errors.New("JWT signing secret must be at least 32 bytes")
	}

	return AppConfig{
		Database: database,
		JWT: JWTConfig{
			SigningSecret: secret,
		},
		FrontendOrigins: loadFrontendOrigins(getenv(frontendOriginsEnv)),
	}, nil
}

func loadFrontendOrigins(value string) []string {
	if strings.TrimSpace(value) == "" {
		return []string{"http://localhost:8080"}
	}
	parts := strings.Split(value, ",")
	origins := make([]string, 0, len(parts))
	for _, part := range parts {
		if origin := strings.TrimSpace(part); origin != "" {
			origins = append(origins, origin)
		}
	}
	return origins
}

func loadDatabaseConfig(getenv func(string) string) (DatabaseConfig, error) {
	dsn := strings.TrimSpace(getenv(databaseDSNEnv))
	if err := validateDatabaseDSN(dsn); err != nil {
		return DatabaseConfig{}, err
	}

	maxOpen, err := positiveInt(getenv, maxOpenConnsEnv, defaultMaxOpenConns)
	if err != nil {
		return DatabaseConfig{}, err
	}
	maxIdle, err := positiveInt(getenv, maxIdleConnsEnv, defaultMaxIdleConns)
	if err != nil {
		return DatabaseConfig{}, err
	}
	if maxIdle > maxOpen {
		return DatabaseConfig{}, errors.New("database idle connection limit cannot exceed open connection limit")
	}

	lifetime, err := positiveDuration(getenv, connMaxLifetimeEnv, defaultConnMaxLifetime)
	if err != nil {
		return DatabaseConfig{}, err
	}
	idleTime, err := positiveDuration(getenv, connMaxIdleTimeEnv, defaultConnMaxIdleTime)
	if err != nil {
		return DatabaseConfig{}, err
	}
	pingTimeout, err := positiveDuration(getenv, databasePingTimeoutEnv, defaultDatabasePingTimeout)
	if err != nil {
		return DatabaseConfig{}, err
	}

	return DatabaseConfig{
		DSN:             dsn,
		MaxOpenConns:    maxOpen,
		MaxIdleConns:    maxIdle,
		ConnMaxLifetime: lifetime,
		ConnMaxIdleTime: idleTime,
		PingTimeout:     pingTimeout,
	}, nil
}

func validateDatabaseDSN(dsn string) error {
	if dsn == "" {
		return errors.New("database connection string is required")
	}

	if strings.Contains(dsn, "=") && !strings.Contains(dsn, "://") {
		return nil
	}

	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Host == "" {
		return errors.New("database connection string is invalid")
	}
	return nil
}

func positiveInt(getenv func(string) string, key string, defaultValue int) (int, error) {
	value := strings.TrimSpace(getenv(key))
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return parsed, nil
}

func positiveDuration(getenv func(string) string, key string, defaultValue time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(getenv(key))
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return parsed, nil
}
