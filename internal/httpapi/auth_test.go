package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/auth"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/checker"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/httpapi/middleware"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/clock"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/logger"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/repository/memory"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/service"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

// testMonitorLimit is MAX_MONITORS_PER_USER in HTTP tests.
const testMonitorLimit = 3

// testAPI is the full router with real services, auth and middleware; only
// storage is in memory.
type testAPI struct {
	router    http.Handler
	clock     *clock.Fake
	tokens    *auth.Tokens
	auth      *service.Auth
	monitors  *memory.Monitors
	checks    *memory.Checks
	incidents *memory.Incidents
	channels  *memory.Channels
	sender    *recordingSender
	// metrics holds the HTTP metrics of router.
	metrics *prometheus.Registry
}

func newTestAPI(t *testing.T, db fakeDB, log *slog.Logger) testAPI {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	tokens := auth.NewTokens(testSecret, time.Hour, clk)
	authSvc, err := service.NewAuth(memory.NewUsers(clk.Now), auth.NewPasswords(bcrypt.MinCost), tokens)
	require.NoError(t, err)
	monitors := memory.NewMonitors(clk.Now)
	checks := memory.NewChecks(monitors)
	incidents := memory.NewIncidents(monitors)
	channels := memory.NewChannels(clk.Now)
	sender := &recordingSender{}
	reg := prometheus.NewRegistry()

	router := NewRouter(Deps{
		BuildInfo: observability.BuildInfo{
			Version: "1.2.3",
			Commit:  "abc123",
			BuiltAt: "2026-01-02T03:04:05Z",
		},
		DB:          db,
		Log:         log,
		Clock:       clk,
		Metrics:     observability.NewHTTPMetrics(reg),
		Auth:        authSvc,
		Monitors:    service.NewMonitors(monitors, checker.NewTargetPolicy(false), clk, testMonitorLimit),
		Checks:      service.NewChecks(monitors, checks, clk),
		Incidents:   service.NewIncidents(incidents),
		Channels:    service.NewChannels(channels, checker.NewTargetPolicy(false), sender, clk),
		Tokens:      tokens,
		AuthLimiter: middleware.NewRateLimiter(10, time.Minute, clk),
	})
	return testAPI{router: router, clock: clk, tokens: tokens, auth: authSvc, monitors: monitors, checks: checks, incidents: incidents, channels: channels, sender: sender, metrics: reg}
}

// userToken registers a user through the service (bypassing the auth rate
// limit, which tests with many users would hit) and returns a valid token.
func (a testAPI) userToken(t *testing.T, email string) string {
	t.Helper()
	u, err := a.auth.Register(t.Context(), service.Credentials{Email: email, Password: "password-1"})
	require.NoError(t, err)
	token, _, err := a.tokens.Issue(u.ID)
	require.NoError(t, err)
	return token
}

// do sends a request. body is sent as is; token, if set, as a bearer token.
func (a testAPI) do(t *testing.T, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.1:5555"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)
	// Every response of every HTTP test must match api/openapi.yaml.
	checkResponse(t, method, path, rec)
	return rec
}

func (a testAPI) register(t *testing.T, email, password string) {
	t.Helper()
	rec := a.do(t, http.MethodPost, "/api/v1/auth/register",
		`{"email":"`+email+`","password":"`+password+`"}`, "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

func (a testAPI) login(t *testing.T, email, password string) string {
	t.Helper()
	rec := a.do(t, http.MethodPost, "/api/v1/auth/login",
		`{"email":"`+email+`","password":"`+password+`"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.AccessToken
}

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestAuthFlow is the happy path: register, log in, call /me.
func TestAuthFlow(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, fakeDB{}, discardLog())

	rec := api.do(t, http.MethodPost, "/api/v1/auth/register",
		`{"email":"Alice@Example.com","password":"password-1"}`, "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var user struct {
		ID        string `json:"id"`
		Email     string `json:"email"`
		CreatedAt string `json:"created_at"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &user))
	assert.Equal(t, "alice@example.com", user.Email)
	assert.Equal(t, "2026-01-02T03:04:05Z", user.CreatedAt)
	assert.NotContains(t, rec.Body.String(), "password", "no password or hash in responses")

	rec = api.do(t, http.MethodPost, "/api/v1/auth/login",
		`{"email":"alice@example.com","password":"password-1"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	var session struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresAt   string `json:"expires_at"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &session))
	assert.Equal(t, "Bearer", session.TokenType)
	assert.Equal(t, "2026-01-02T04:04:05Z", session.ExpiresAt)

	rec = api.do(t, http.MethodGet, "/api/v1/me", "", session.AccessToken)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t,
		`{"id":"`+user.ID+`","email":"alice@example.com","created_at":"2026-01-02T03:04:05Z"}`,
		rec.Body.String())
}

func TestRegisterErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "duplicate email",
			body:       `{"email":"taken@example.com","password":"password-2"}`,
			wantStatus: http.StatusConflict,
			wantBody:   `{"error":{"code":"conflict","message":"Resource already exists"}}`,
		},
		{
			name:       "invalid fields",
			body:       `{"email":"nope","password":"short"}`,
			wantStatus: http.StatusBadRequest,
			wantBody: `{"error":{"code":"validation_failed","message":"Invalid request","details":[
				{"field":"email","issue":"must be a valid email address"},
				{"field":"password","issue":"must be at least 8 characters"}]}}`,
		},
		{
			name:       "empty body",
			body:       ``,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":{"code":"validation_failed","message":"Invalid request","details":[{"field":"body","issue":"is required"}]}}`,
		},
		{
			name:       "malformed JSON",
			body:       `{"email":`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":{"code":"validation_failed","message":"Invalid request","details":[{"field":"body","issue":"is not valid JSON: unexpected EOF"}]}}`,
		},
		{
			name:       "unknown field",
			body:       `{"email":"a@example.com","pasword":"password-1"}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":{"code":"validation_failed","message":"Invalid request","details":[{"field":"pasword","issue":"is not a known field"}]}}`,
		},
		{
			name:       "wrong type",
			body:       `{"email":42,"password":"password-1"}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":{"code":"validation_failed","message":"Invalid request","details":[{"field":"email","issue":"must be a string"}]}}`,
		},
		{
			name:       "two objects",
			body:       `{"email":"a@example.com","password":"password-1"}{}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":{"code":"validation_failed","message":"Invalid request","details":[{"field":"body","issue":"must contain a single JSON object"}]}}`,
		},
		{
			name:       "body over 1 MiB",
			body:       `{"email":"` + strings.Repeat("a", middleware.MaxBodyBytes) + `"}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":{"code":"validation_failed","message":"Invalid request","details":[{"field":"body","issue":"must be at most 1048576 bytes"}]}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			api := newTestAPI(t, fakeDB{}, discardLog())
			api.register(t, "taken@example.com", "password-1")

			rec := api.do(t, http.MethodPost, "/api/v1/auth/register", tt.body, "")

			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.JSONEq(t, tt.wantBody, rec.Body.String())
		})
	}
}

func TestLoginErrors(t *testing.T) {
	t.Parallel()
	unauthorized := `{"error":{"code":"unauthorized","message":"Authentication required"}}`

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "wrong password",
			body:       `{"email":"alice@example.com","password":"password-2"}`,
			wantStatus: http.StatusUnauthorized,
			wantBody:   unauthorized,
		},
		{
			// Same response as a wrong password: emails cannot be probed.
			name:       "unknown email",
			body:       `{"email":"bob@example.com","password":"password-1"}`,
			wantStatus: http.StatusUnauthorized,
			wantBody:   unauthorized,
		},
		{
			name:       "missing password",
			body:       `{"email":"alice@example.com"}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":{"code":"validation_failed","message":"Invalid request","details":[{"field":"password","issue":"is required"}]}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			api := newTestAPI(t, fakeDB{}, discardLog())
			api.register(t, "alice@example.com", "password-1")

			rec := api.do(t, http.MethodPost, "/api/v1/auth/login", tt.body, "")

			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.JSONEq(t, tt.wantBody, rec.Body.String())
		})
	}
}

func TestMeRejectsBadTokens(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, fakeDB{}, discardLog())
	api.register(t, "alice@example.com", "password-1")
	valid := api.login(t, "alice@example.com", "password-1")

	otherKey := auth.NewTokens([]byte("another-secret-another-secret-xx"), time.Hour, api.clock)
	forged, _, err := otherKey.Issue(uuid.New())
	require.NoError(t, err)
	// Correctly signed, but for a user that does not exist (any more).
	orphan, _, err := api.tokens.Issue(uuid.New())
	require.NoError(t, err)

	tests := []struct {
		name  string
		token string
	}{
		{name: "missing", token: ""},
		{name: "garbage", token: "not-a-jwt"},
		{name: "tampered", token: valid[:len(valid)-2] + "xx"},
		{name: "signed with another key", token: forged},
		{name: "unknown user", token: orphan},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := api.do(t, http.MethodGet, "/api/v1/me", "", tt.token)

			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.JSONEq(t, `{"error":{"code":"unauthorized","message":"Authentication required"}}`, rec.Body.String())
		})
	}
}

func TestMeRejectsExpiredToken(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, fakeDB{}, discardLog())
	api.register(t, "alice@example.com", "password-1")
	token := api.login(t, "alice@example.com", "password-1")

	api.clock.Advance(time.Hour + time.Second)
	rec := api.do(t, http.MethodGet, "/api/v1/me", "", token)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuthRateLimit(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	api := newTestAPI(t, fakeDB{}, logger.New(&logs, logger.Options{Format: logger.FormatJSON, Level: slog.LevelInfo}))

	// Both endpoints share the budget of 10 requests per minute per IP. The
	// 11th request goes to login.
	codes := make([]int, 0, 11)
	for i := range 11 {
		path := "/api/v1/auth/login"
		if i%2 == 1 {
			path = "/api/v1/auth/register"
		}
		codes = append(codes, api.do(t, http.MethodPost, path, `{}`, "").Code)
	}

	assert.NotContains(t, codes[:10], http.StatusTooManyRequests)
	assert.Equal(t, http.StatusTooManyRequests, codes[10])
	// The rejected request is logged under its full route template.
	assert.Contains(t, logs.String(), `"status":429`)
	assert.Contains(t, logs.String(), `"route":"/api/v1/auth/login"`)

	// /me is not rate-limited.
	rec := api.do(t, http.MethodGet, "/api/v1/me", "", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// An unexpected error becomes a 500 without details, and the log has the
// cause with the request ID returned to the client.
func TestInternalErrorIsLoggedWithRequestID(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	log := logger.New(&logs, logger.Options{Format: logger.FormatJSON, Level: slog.LevelInfo})
	api := newTestAPI(t, fakeDB{}, log)
	api.register(t, "alice@example.com", "password-1")
	token := api.login(t, "alice@example.com", "password-1")
	// Swap in a router whose service always fails.
	api.router = NewRouter(Deps{
		Log: log, Clock: api.clock, DB: fakeDB{}, Metrics: observability.NewHTTPMetrics(prometheus.NewRegistry()),
		Auth: failingAuth{}, Tokens: api.tokens,
		AuthLimiter: middleware.NewRateLimiter(10, time.Minute, api.clock),
	})

	rec := api.do(t, http.MethodGet, "/api/v1/me", "", token)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"error":{"code":"internal","message":"Internal server error"}}`, rec.Body.String())
	assert.Contains(t, logs.String(), "database exploded")
	assert.Contains(t, logs.String(), rec.Header().Get(middleware.RequestIDHeader))
}

// failingAuth is an AuthService whose storage is down.
type failingAuth struct{}

var errDatabaseExploded = errors.New("database exploded")

func (failingAuth) Register(context.Context, service.Credentials) (domain.User, error) {
	return domain.User{}, errDatabaseExploded
}

func (failingAuth) Login(context.Context, service.Credentials) (service.Session, error) {
	return service.Session{}, errDatabaseExploded
}

func (failingAuth) Me(context.Context, uuid.UUID) (domain.User, error) {
	return domain.User{}, errDatabaseExploded
}
