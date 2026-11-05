package ops

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func ok(context.Context) error   { return nil }
func fail(context.Context) error { return errors.New("down") }

func TestHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		checks     []Check
		path       string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "liveness ignores checks",
			checks:     []Check{{Name: "database", Fn: fail}},
			path:       "/healthz",
			wantStatus: http.StatusOK,
			wantBody:   `{"status":"ok"}`,
		},
		{
			name:       "ready",
			checks:     []Check{{Name: "database", Fn: ok}, {Name: "scheduler", Fn: ok}},
			path:       "/readyz",
			wantStatus: http.StatusOK,
			wantBody:   `{"status":"ok","checks":{"database":"ok","scheduler":"ok"}}`,
		},
		{
			name:       "one check fails",
			checks:     []Check{{Name: "database", Fn: ok}, {Name: "scheduler", Fn: fail}},
			path:       "/readyz",
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `{"status":"unavailable","checks":{"database":"ok","scheduler":"unavailable"}}`,
		},
		{
			name:       "unknown path",
			path:       "/nope",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()

			Handler(slog.New(slog.DiscardHandler), Options{Checks: tt.checks}).
				ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, http.NoBody))

			assert.Equal(t, tt.wantStatus, rec.Code)
			if tt.wantBody != "" {
				assert.JSONEq(t, tt.wantBody, rec.Body.String())
			}
		})
	}
}

func TestHandlerOptionalEndpoints(t *testing.T) {
	t.Parallel()
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("uptime_build_info 1\n"))
	})
	get := func(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody))
		return rec
	}

	t.Run("enabled", func(t *testing.T) {
		t.Parallel()
		h := Handler(slog.New(slog.DiscardHandler), Options{Metrics: metrics, Pprof: true})

		rec := get(t, h, "/metrics")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "uptime_build_info 1\n", rec.Body.String())
		assert.Equal(t, http.StatusOK, get(t, h, "/debug/pprof/").Code)
		assert.Equal(t, http.StatusOK, get(t, h, "/debug/pprof/goroutine?debug=1").Code)
	})

	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		h := Handler(slog.New(slog.DiscardHandler), Options{})

		assert.Equal(t, http.StatusNotFound, get(t, h, "/metrics").Code)
		assert.Equal(t, http.StatusNotFound, get(t, h, "/debug/pprof/").Code)
	})
}
