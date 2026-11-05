package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// fakeDB is a Pinger that returns err.
type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func newTestRouter(t *testing.T, db fakeDB) http.Handler {
	t.Helper()
	return newTestAPI(t, db, slog.New(slog.NewTextHandler(io.Discard, nil))).router
}

func TestRouter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		db         fakeDB
		method     string
		path       string
		wantStatus int
		wantBody   string // JSON; empty means "don't check"
	}{
		{
			name:       "liveness",
			method:     http.MethodGet,
			path:       "/healthz",
			wantStatus: http.StatusOK,
			wantBody:   `{"status":"ok"}`,
		},
		{
			// Liveness must not depend on the database (see handler.Healthz).
			name:       "liveness while the database is down",
			db:         fakeDB{err: errors.New("connection refused")},
			method:     http.MethodGet,
			path:       "/healthz",
			wantStatus: http.StatusOK,
			wantBody:   `{"status":"ok"}`,
		},
		{
			name:       "readiness",
			method:     http.MethodGet,
			path:       "/readyz",
			wantStatus: http.StatusOK,
			wantBody:   `{"status":"ok","checks":{"database":"ok"}}`,
		},
		{
			// The error text stays in the log: this port is public.
			name:       "readiness while the database is down",
			db:         fakeDB{err: errors.New("connection refused")},
			method:     http.MethodGet,
			path:       "/readyz",
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `{"status":"unavailable","checks":{"database":"unavailable"}}`,
		},
		{
			name:       "version",
			method:     http.MethodGet,
			path:       "/version",
			wantStatus: http.StatusOK,
			wantBody:   `{"version":"1.2.3","commit":"abc123","built_at":"2026-01-02T03:04:05Z"}`,
		},
		{
			name:       "wrong method",
			method:     http.MethodPost,
			path:       "/healthz",
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "unknown route",
			method:     http.MethodGet,
			path:       "/nope",
			wantStatus: http.StatusNotFound,
		},
		{
			// Metrics live on the ops port only, never on the public one.
			name:       "metrics are not public",
			method:     http.MethodGet,
			path:       "/metrics",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, http.NoBody)
			rec := httptest.NewRecorder()

			newTestRouter(t, tt.db).ServeHTTP(rec, req)
			checkResponse(t, tt.method, tt.path, rec)

			assert.Equal(t, tt.wantStatus, rec.Code)
			if tt.wantBody != "" {
				assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
				assert.JSONEq(t, tt.wantBody, rec.Body.String())
			}
		})
	}
}
