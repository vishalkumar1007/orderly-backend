package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Config holds process configuration loaded from the environment.
type Config struct {
	HTTPAddr         string
	AppEnv           string
	LogLevel         string
	DatabaseURL      string
	BaseDomain       string
	FrontendPort     string
	JWTAccessSecret  string
	JWTRefreshSecret string
	// ConfigEncryptionKey seals provider credentials (SMTP password, storage
	// secret key, AI API key) at rest. When empty, secrets cannot be stored
	// safely and the configuration endpoints refuse to accept them.
	ConfigEncryptionKey string
	AccessTokenTTL      time.Duration
	RefreshTokenTTL     time.Duration
}

// Load reads configuration from environment variables.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:         getenv("HTTP_ADDR", ":8080"),
		AppEnv:           getenv("APP_ENV", "development"),
		LogLevel:         getenv("LOG_LEVEL", "info"),
		DatabaseURL:      strings.TrimSpace(os.Getenv("DATABASE_URL")),
		BaseDomain:       getenv("BASE_DOMAIN", "localhost"),
		FrontendPort:     getenv("FRONTEND_PORT", "5173"),
		JWTAccessSecret:  getenv("JWT_ACCESS_SECRET", "dev-access-secret-change-me"),
		JWTRefreshSecret: getenv("JWT_REFRESH_SECRET", "dev-refresh-secret-change-me"),
		// No default on purpose: silently inventing a key would mean every
		// deployment shared it, and rotating it would orphan every stored
		// secret. A missing key degrades loudly instead.
		ConfigEncryptionKey: strings.TrimSpace(os.Getenv("CONFIG_ENCRYPTION_KEY")),
		AccessTokenTTL:      15 * time.Minute,
		RefreshTokenTTL:     7 * 24 * time.Hour,
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}

	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
