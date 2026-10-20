package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Config holds process configuration loaded from the environment.
type Config struct {
	// SignInTestFallback lets automated tests sign customers in without an
	// SMS or mail account. Ignored when APP_ENV is production.
	SignInTestFallback bool
	HTTPAddr           string
	AppEnv             string
	LogLevel           string
	DatabaseURL        string
	BaseDomain         string
	FrontendPort       string
	JWTAccessSecret    string
	JWTRefreshSecret   string
	// ConfigEncryptionKey seals provider credentials (SMTP password, storage
	// secret key, AI API key) at rest. When empty, secrets cannot be stored
	// safely and the configuration endpoints refuse to accept them.
	ConfigEncryptionKey string
	// PaymentsV2 switches on the provider/webhook payment path (plan P-A+).
	// Off by default so behaviour stays exactly as before until opted in.
	// HealthDetailToken, when set, hides version / schema / latency from the
	// public /health endpoints unless the caller sends it as X-Health-Token.
	// Unset (local dev) shows everything.
	HealthDetailToken string
	PaymentsV2        bool
	// PaymentsSandboxSecret signs the dummy gateway's callbacks. Falls back to a
	// key derived from JWT_ACCESS_SECRET when unset; set it in production so
	// rotating one does not disturb the other.
	PaymentsSandboxSecret string
	// PaymentsRateLimit throttles the public payment and webhook routes. On by
	// default in production, off elsewhere so local test runs are not throttled.
	PaymentsRateLimit bool
	AccessTokenTTL    time.Duration
	RefreshTokenTTL   time.Duration
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
		ConfigEncryptionKey:   strings.TrimSpace(os.Getenv("CONFIG_ENCRYPTION_KEY")),
		HealthDetailToken:     strings.TrimSpace(os.Getenv("HEALTH_DETAIL_TOKEN")),
		PaymentsV2:            os.Getenv("PAYMENTS_V2") == "true",
		SignInTestFallback:    os.Getenv("CUSTOMER_SIGNIN_TEST_FALLBACK") == "true",
		PaymentsSandboxSecret: strings.TrimSpace(os.Getenv("PAYMENTS_SANDBOX_SECRET")),
		AccessTokenTTL:        15 * time.Minute,
		RefreshTokenTTL:       7 * 24 * time.Hour,
	}

	switch strings.ToLower(strings.TrimSpace(os.Getenv("PAYMENTS_RATE_LIMIT"))) {
	case "true", "on", "1":
		cfg.PaymentsRateLimit = true
	case "false", "off", "0":
		cfg.PaymentsRateLimit = false
	default:
		cfg.PaymentsRateLimit = cfg.AppEnv == "production"
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
