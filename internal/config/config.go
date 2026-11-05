// Package config loads service configuration from environment variables and
// validates it at startup, so a misconfigured process fails fast instead of
// misbehaving later.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/caarlos0/env/v11"
)

// Env is the deployment environment. It selects the log format.
type Env string

// Supported deployment environments.
const (
	EnvDev  Env = "dev"
	EnvProd Env = "prod"
)

// Common holds settings shared by all binaries.
type Common struct {
	Env             Env           `env:"APP_ENV"          envDefault:"dev"`
	LogLevel        slog.Level    `env:"LOG_LEVEL"        envDefault:"info"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"25s"`
}

// Database holds the PostgreSQL connection pool settings.
type Database struct {
	// URL is a libpq connection string or URL. It contains the password, so
	// it must never be logged.
	URL      string `env:"DATABASE_URL,required,notEmpty"`
	MaxConns int32  `env:"DB_MAX_CONNS"                    envDefault:"10"`
}

// MinJWTSecretLen is the minimum JWT_SECRET length in bytes. HS256 uses a
// 256-bit key; a shorter secret is easier to brute-force offline from a
// single leaked token.
const MinJWTSecretLen = 32

// Auth holds authentication settings.
type Auth struct {
	// JWTSecret signs access tokens. Anyone who knows it can forge a token
	// for any user, so it must never be logged.
	JWTSecret      string        `env:"JWT_SECRET,required,notEmpty"`
	AccessTokenTTL time.Duration `env:"ACCESS_TOKEN_TTL"                envDefault:"12h"`
}

// Notify holds notification delivery settings, shared by the API (test
// notifications) and the worker (the outbox).
type Notify struct {
	// TelegramBotToken is the Bot API token. Empty disables Telegram:
	// telegram channels can still be created, but every delivery to them
	// fails. Anyone who knows it controls the bot, so it is never logged.
	TelegramBotToken string `env:"TELEGRAM_BOT_TOKEN"`
}

// API is the configuration of cmd/api.
type API struct {
	Common
	DB     Database
	Auth   Auth
	Notify Notify

	HTTPAddr string `env:"HTTP_ADDR" envDefault:":8080"`
	// OpsAddr serves /metrics (and pprof). It must not be published outside
	// the host network.
	OpsAddr string `env:"OPS_ADDR" envDefault:":9090"`
	// PprofEnabled serves the Go profiler on the ops port.
	PprofEnabled bool `env:"PPROF_ENABLED" envDefault:"false"`
	// TrustProxyHeaders makes the API take the client IP (for rate limiting)
	// from X-Forwarded-For. Enable it only behind a reverse proxy that sets
	// that header; otherwise clients can spoof their IP and dodge the limit.
	TrustProxyHeaders bool `env:"TRUST_PROXY_HEADERS" envDefault:"false"`

	MaxMonitorsPerUser int `env:"MAX_MONITORS_PER_USER" envDefault:"20"`
	// AllowPrivateTargets lets monitors point at loopback and private
	// networks. For local development only: in production it turns the
	// service into a proxy into the internal network (SSRF).
	AllowPrivateTargets bool `env:"ALLOW_PRIVATE_TARGETS" envDefault:"false"`
}

// Migrate is the configuration of cmd/migrate. It uses one connection, so
// the pool size does not apply.
type Migrate struct {
	Common

	DatabaseURL string `env:"DATABASE_URL,required,notEmpty"`
}

// LoadAPI reads the API configuration from the process environment.
func LoadAPI() (API, error) {
	return loadAPI(nil)
}

// loadAPI parses environ, or the process environment when environ is nil.
// Tests pass a map instead of mutating the real environment.
func loadAPI(environ map[string]string) (API, error) {
	cfg, err := env.ParseAsWithOptions[API](env.Options{Environment: environ})
	if err != nil {
		return API{}, fmt.Errorf("config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return API{}, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

// Worker is the configuration of cmd/worker.
type Worker struct {
	Common
	DB     Database
	Notify Notify

	// OpsAddr serves /healthz, /readyz and /metrics. It must not be
	// published outside the host network.
	OpsAddr string `env:"OPS_ADDR" envDefault:":9091"`
	// Concurrency is the number of checks running at once.
	Concurrency int `env:"WORKER_CONCURRENCY" envDefault:"20"`
	// BatchSize is the maximum number of monitors claimed per tick.
	BatchSize int `env:"SCHEDULER_BATCH_SIZE" envDefault:"50"`
	// RetentionDays is how long check results are kept.
	RetentionDays int `env:"CHECKS_RETENTION_DAYS" envDefault:"30"`
	// AllowPrivateTargets: see API.AllowPrivateTargets. The worker enforces
	// it when connecting; the API when a monitor is saved.
	AllowPrivateTargets bool `env:"ALLOW_PRIVATE_TARGETS" envDefault:"false"`
}

// LoadWorker reads the cmd/worker configuration from the process environment.
func LoadWorker() (Worker, error) {
	return loadWorker(nil)
}

func loadWorker(environ map[string]string) (Worker, error) {
	cfg, err := env.ParseAsWithOptions[Worker](env.Options{Environment: environ})
	if err != nil {
		return Worker{}, fmt.Errorf("config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return Worker{}, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

func (c Worker) validate() error {
	errs := []error{c.Common.validate(), c.DB.validate()}
	if _, _, err := net.SplitHostPort(c.OpsAddr); err != nil {
		errs = append(errs, fmt.Errorf("OPS_ADDR must be host:port, got %q: %w", c.OpsAddr, err))
	}
	if c.Concurrency < 1 {
		errs = append(errs, fmt.Errorf("WORKER_CONCURRENCY must be at least 1, got %d", c.Concurrency))
	}
	if c.BatchSize < 1 {
		errs = append(errs, fmt.Errorf("SCHEDULER_BATCH_SIZE must be at least 1, got %d", c.BatchSize))
	}
	if c.RetentionDays < 1 {
		errs = append(errs, fmt.Errorf("CHECKS_RETENTION_DAYS must be at least 1, got %d", c.RetentionDays))
	}
	return errors.Join(errs...)
}

// LoadMigrate reads the cmd/migrate configuration from the process environment.
func LoadMigrate() (Migrate, error) {
	return loadMigrate(nil)
}

func loadMigrate(environ map[string]string) (Migrate, error) {
	cfg, err := env.ParseAsWithOptions[Migrate](env.Options{Environment: environ})
	if err != nil {
		return Migrate{}, fmt.Errorf("config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return Migrate{}, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

// validate checks values that parse fine but are unusable. It reports all
// problems at once, so they can be fixed in one go.
func (c Common) validate() error {
	var errs []error
	if c.Env != EnvDev && c.Env != EnvProd {
		errs = append(errs, fmt.Errorf("APP_ENV must be %q or %q, got %q", EnvDev, EnvProd, c.Env))
	}
	if c.ShutdownTimeout <= 0 {
		errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT must be positive, got %s", c.ShutdownTimeout))
	}
	return errors.Join(errs...)
}

func (d Database) validate() error {
	if d.MaxConns < 1 {
		return fmt.Errorf("DB_MAX_CONNS must be at least 1, got %d", d.MaxConns)
	}
	return nil
}

func (a Auth) validate() error {
	var errs []error
	if len(a.JWTSecret) < MinJWTSecretLen {
		// The value itself is not reported: it is a secret.
		errs = append(errs, fmt.Errorf("JWT_SECRET must be at least %d bytes, got %d", MinJWTSecretLen, len(a.JWTSecret)))
	}
	if a.AccessTokenTTL <= 0 {
		errs = append(errs, fmt.Errorf("ACCESS_TOKEN_TTL must be positive, got %s", a.AccessTokenTTL))
	}
	return errors.Join(errs...)
}

func (c API) validate() error {
	errs := []error{c.Common.validate(), c.DB.validate(), c.Auth.validate()}
	if c.MaxMonitorsPerUser < 1 {
		errs = append(errs, fmt.Errorf("MAX_MONITORS_PER_USER must be at least 1, got %d", c.MaxMonitorsPerUser))
	}
	if _, _, err := net.SplitHostPort(c.HTTPAddr); err != nil {
		errs = append(errs, fmt.Errorf("HTTP_ADDR must be host:port, got %q: %w", c.HTTPAddr, err))
	}
	if _, _, err := net.SplitHostPort(c.OpsAddr); err != nil {
		errs = append(errs, fmt.Errorf("OPS_ADDR must be host:port, got %q: %w", c.OpsAddr, err))
	}
	return errors.Join(errs...)
}
