package config

import (
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDSN = "postgres://uptime:secret@localhost:5432/uptime"

// testSecret is 32 bytes, the minimum JWT_SECRET length.
const testSecret = "0123456789abcdef0123456789abcdef"

// unset marks a variable that withRequired must leave out of the environment.
const unset = "\x00unset"

// withRequired returns environ plus valid required variables, so error cases fail
// only for the reason they test. A value of unset removes the variable.
func withRequired(environ map[string]string) map[string]string {
	out := map[string]string{"DATABASE_URL": testDSN, "JWT_SECRET": testSecret}
	for k, v := range environ {
		if v == unset {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

func TestLoadAPI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		environ map[string]string
		want    API
	}{
		{
			name:    "defaults",
			environ: map[string]string{},
			want: API{
				Common: Common{
					Env:             EnvDev,
					LogLevel:        slog.LevelInfo,
					ShutdownTimeout: 25 * time.Second,
				},
				DB:                 Database{URL: testDSN, MaxConns: 10},
				Auth:               Auth{JWTSecret: testSecret, AccessTokenTTL: 12 * time.Hour},
				HTTPAddr:           ":8080",
				OpsAddr:            ":9090",
				MaxMonitorsPerUser: 20,
			},
		},
		{
			name: "overrides",
			environ: map[string]string{
				"APP_ENV":               "prod",
				"LOG_LEVEL":             "debug",
				"SHUTDOWN_TIMEOUT":      "5s",
				"HTTP_ADDR":             "127.0.0.1:9000",
				"OPS_ADDR":              "127.0.0.1:9001",
				"PPROF_ENABLED":         "true",
				"DATABASE_URL":          testDSN,
				"DB_MAX_CONNS":          "4",
				"ACCESS_TOKEN_TTL":      "1h",
				"TRUST_PROXY_HEADERS":   "true",
				"MAX_MONITORS_PER_USER": "5",
				"TELEGRAM_BOT_TOKEN":    "123:abc",
				"ALLOW_PRIVATE_TARGETS": "true",
			},
			want: API{
				Common: Common{
					Env:             EnvProd,
					LogLevel:        slog.LevelDebug,
					ShutdownTimeout: 5 * time.Second,
				},
				DB:                  Database{URL: testDSN, MaxConns: 4},
				Auth:                Auth{JWTSecret: testSecret, AccessTokenTTL: time.Hour},
				Notify:              Notify{TelegramBotToken: "123:abc"},
				TrustProxyHeaders:   true,
				HTTPAddr:            "127.0.0.1:9000",
				OpsAddr:             "127.0.0.1:9001",
				PprofEnabled:        true,
				MaxMonitorsPerUser:  5,
				AllowPrivateTargets: true,
			},
		},
		{
			name:    "log level is case-insensitive",
			environ: map[string]string{"LOG_LEVEL": "WARN"},
			want: API{
				Common: Common{
					Env:             EnvDev,
					LogLevel:        slog.LevelWarn,
					ShutdownTimeout: 25 * time.Second,
				},
				DB:                 Database{URL: testDSN, MaxConns: 10},
				Auth:               Auth{JWTSecret: testSecret, AccessTokenTTL: 12 * time.Hour},
				HTTPAddr:           ":8080",
				OpsAddr:            ":9090",
				MaxMonitorsPerUser: 20,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := loadAPI(withRequired(tt.environ))

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLoadAPIErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		environ map[string]string
		// wantErr lists substrings that must all appear in the error.
		wantErr []string
	}{
		{
			name:    "unknown environment",
			environ: map[string]string{"APP_ENV": "staging"},
			wantErr: []string{"APP_ENV"},
		},
		// Parse errors come from caarlos0/env, which names the struct field
		// rather than the variable, along with the rejected value.
		{
			name:    "unknown log level",
			environ: map[string]string{"LOG_LEVEL": "verbose"},
			wantErr: []string{"LogLevel", "verbose"},
		},
		{
			name:    "malformed duration",
			environ: map[string]string{"SHUTDOWN_TIMEOUT": "soon"},
			wantErr: []string{"ShutdownTimeout", "soon"},
		},
		{
			name:    "zero shutdown timeout",
			environ: map[string]string{"SHUTDOWN_TIMEOUT": "0s"},
			wantErr: []string{"SHUTDOWN_TIMEOUT must be positive"},
		},
		{
			name:    "listen address without port",
			environ: map[string]string{"HTTP_ADDR": "8080"},
			wantErr: []string{"HTTP_ADDR must be host:port"},
		},
		{
			name:    "ops address without port",
			environ: map[string]string{"OPS_ADDR": "9090"},
			wantErr: []string{"OPS_ADDR must be host:port"},
		},
		{
			name: "all validation errors are reported together",
			environ: map[string]string{
				"APP_ENV":          "staging",
				"SHUTDOWN_TIMEOUT": "-1s",
				"HTTP_ADDR":        "localhost",
				"DB_MAX_CONNS":     "0",
			},
			wantErr: []string{"APP_ENV", "SHUTDOWN_TIMEOUT", "HTTP_ADDR", "DB_MAX_CONNS"},
		},
		{
			name:    "example secret in production",
			environ: map[string]string{"APP_ENV": "prod", "JWT_SECRET": ExampleJWTSecret},
			wantErr: []string{"JWT_SECRET is the public example value"},
		},
		{
			name:    "database URL is required",
			environ: map[string]string{"DATABASE_URL": unset},
			wantErr: []string{"DATABASE_URL"},
		},
		{
			name:    "database URL must not be empty",
			environ: map[string]string{"DATABASE_URL": ""},
			wantErr: []string{"DATABASE_URL"},
		},
		{
			name:    "zero pool size",
			environ: map[string]string{"DB_MAX_CONNS": "0"},
			wantErr: []string{"DB_MAX_CONNS must be at least 1"},
		},
		{
			name:    "JWT secret is required",
			environ: map[string]string{"JWT_SECRET": unset},
			wantErr: []string{"JWT_SECRET"},
		},
		{
			name:    "short JWT secret",
			environ: map[string]string{"JWT_SECRET": "too-short"},
			wantErr: []string{"JWT_SECRET must be at least 32 bytes, got 9"},
		},
		{
			name:    "zero token TTL",
			environ: map[string]string{"ACCESS_TOKEN_TTL": "0s"},
			wantErr: []string{"ACCESS_TOKEN_TTL must be positive"},
		},
		{
			name:    "zero monitor limit",
			environ: map[string]string{"MAX_MONITORS_PER_USER": "0"},
			wantErr: []string{"MAX_MONITORS_PER_USER must be at least 1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := loadAPI(withRequired(tt.environ))

			require.Error(t, err)
			for _, want := range tt.wantErr {
				assert.ErrorContains(t, err, want)
			}
		})
	}
}

func TestLoadMigrate(t *testing.T) {
	t.Parallel()

	got, err := loadMigrate(map[string]string{"DATABASE_URL": testDSN, "LOG_LEVEL": "debug"})

	require.NoError(t, err)
	assert.Equal(t, Migrate{
		Common: Common{
			Env:             EnvDev,
			LogLevel:        slog.LevelDebug,
			ShutdownTimeout: 25 * time.Second,
		},
		DatabaseURL: testDSN,
	}, got)
}

func TestLoadMigrateErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		environ map[string]string
		wantErr string
	}{
		{name: "database URL is required", environ: map[string]string{}, wantErr: "DATABASE_URL"},
		{
			name:    "common settings are validated",
			environ: map[string]string{"DATABASE_URL": testDSN, "APP_ENV": "staging"},
			wantErr: "APP_ENV",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := loadMigrate(tt.environ)

			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestLoadWorker(t *testing.T) {
	t.Parallel()

	defaults, err := loadWorker(map[string]string{"DATABASE_URL": testDSN})
	require.NoError(t, err)
	assert.Equal(t, Worker{
		Common:        Common{Env: EnvDev, LogLevel: slog.LevelInfo, ShutdownTimeout: 25 * time.Second},
		DB:            Database{URL: testDSN, MaxConns: 10},
		OpsAddr:       ":9091",
		Concurrency:   20,
		BatchSize:     50,
		RetentionDays: 30,
	}, defaults)

	custom, err := loadWorker(map[string]string{
		"DATABASE_URL":          testDSN,
		"OPS_ADDR":              "127.0.0.1:19091",
		"WORKER_CONCURRENCY":    "5",
		"SCHEDULER_BATCH_SIZE":  "10",
		"CHECKS_RETENTION_DAYS": "7",
		"TELEGRAM_BOT_TOKEN":    "123:abc",
		"ALLOW_PRIVATE_TARGETS": "true",
	})
	require.NoError(t, err)
	assert.Equal(t, "123:abc", custom.Notify.TelegramBotToken)
	assert.Equal(t, "127.0.0.1:19091", custom.OpsAddr)
	assert.Equal(t, 5, custom.Concurrency)
	assert.Equal(t, 10, custom.BatchSize)
	assert.Equal(t, 7, custom.RetentionDays)
	assert.True(t, custom.AllowPrivateTargets)
}

func TestLoadWorkerErrors(t *testing.T) {
	t.Parallel()

	_, err := loadWorker(map[string]string{
		"DATABASE_URL":          testDSN,
		"OPS_ADDR":              "9091",
		"WORKER_CONCURRENCY":    "0",
		"SCHEDULER_BATCH_SIZE":  "0",
		"CHECKS_RETENTION_DAYS": "0",
	})

	require.Error(t, err)
	for _, want := range []string{"OPS_ADDR", "WORKER_CONCURRENCY", "SCHEDULER_BATCH_SIZE", "CHECKS_RETENTION_DAYS"} {
		assert.ErrorContains(t, err, want)
	}

	_, err = loadWorker(map[string]string{})
	assert.ErrorContains(t, err, "DATABASE_URL")
}

// The example secret is fine for local development (dev is the default).
func TestLoadAPIExampleSecretInDev(t *testing.T) {
	t.Parallel()

	cfg, err := loadAPI(withRequired(map[string]string{"JWT_SECRET": ExampleJWTSecret}))

	require.NoError(t, err)
	assert.Equal(t, ExampleJWTSecret, cfg.Auth.JWTSecret)
}

// .env.example must hold exactly ExampleJWTSecret, or the production guard
// would not recognise it.
func TestEnvExampleUsesExampleSecret(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../.env.example")
	require.NoError(t, err)

	assert.Contains(t, strings.Split(string(data), "\n"), "JWT_SECRET="+ExampleJWTSecret)
}
