package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability/obstest"
)

// TestMetricsCardinality sends requests with IDs, random paths and odd
// methods through the full router and checks that the HTTP metrics only
// ever carry route templates, "unmatched" and known methods: the number of
// series must not grow with users, monitors or junk traffic.
func TestMetricsCardinality(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, fakeDB{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	token := api.userToken(t, "alice@example.com")

	// Every route template of the router, plus the fallback.
	allowedRoutes := map[string]bool{observability.RouteUnmatched: true}
	err := chi.Walk(api.router.(chi.Routes), func(_, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		allowedRoutes[route] = true
		return nil
	})
	require.NoError(t, err)

	requests := []struct{ method, path, token string }{
		{http.MethodGet, "/version", ""},
		{http.MethodGet, "/healthz", ""},
		{http.MethodGet, "/readyz", ""},
		{http.MethodGet, "/nope/" + uuid.NewString(), ""},
		{http.MethodGet, "/api/v1/" + uuid.NewString(), token},
		{"PROPFIND", "/api/v1/monitors", token},
		{"X-" + uuid.NewString(), "/version", ""},
		{http.MethodPost, "/healthz", ""},
		{http.MethodGet, "/api/v1/me", ""},
		{http.MethodGet, "/api/v1/me", token},
	}
	for range 3 {
		id := uuid.NewString()
		requests = append(requests,
			struct{ method, path, token string }{http.MethodGet, "/api/v1/monitors/" + id, token},
			struct{ method, path, token string }{http.MethodGet, "/api/v1/monitors/" + id + "/checks", token},
			struct{ method, path, token string }{http.MethodDelete, "/api/v1/channels/" + id, token},
			struct{ method, path, token string }{http.MethodGet, "/api/v1/monitors/" + id + "/nope", token},
		)
	}
	for _, r := range requests {
		api.do(t, r.method, r.path, "", r.token)
	}

	families, err := api.metrics.Gather()
	require.NoError(t, err)
	seenRoutes := map[string]bool{}
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "route":
					assert.True(t, allowedRoutes[l.GetValue()], "route label %q is not a route template", l.GetValue())
					seenRoutes[l.GetValue()] = true
				case "method":
					assert.Contains(t, []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "other"}, l.GetValue())
				}
			}
		}
	}

	assert.True(t, seenRoutes["/api/v1/monitors/{id}"])
	assert.True(t, seenRoutes[observability.RouteUnmatched])
	assert.False(t, seenRoutes["/healthz"], "probes are not counted")
	assert.False(t, seenRoutes["/readyz"], "probes are not counted")
	// Three different monitor IDs, one series.
	assert.InDelta(t, 3, obstest.Value(t, api.metrics, "uptime_http_requests_total",
		"method", "GET", "route", "/api/v1/monitors/{id}", "code", "404"), 0)
	assert.InDelta(t, 1, obstest.Value(t, api.metrics, "uptime_http_requests_total",
		"method", "GET", "route", "/api/v1/me", "code", "401"), 0)
	assert.InDelta(t, 0, obstest.Value(t, api.metrics, "uptime_http_requests_in_flight"), 0)

	problems, err := testutil.GatherAndLint(api.metrics)
	require.NoError(t, err)
	assert.Empty(t, problems)
}

// TestMetricsCountPanics checks that a panic in a handler is counted as a
// 500, so it shows up in the error rate.
func TestMetricsCountPanics(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, fakeDB{}, slog.New(slog.DiscardHandler))
	router, ok := api.router.(chi.Router)
	require.True(t, ok)
	router.Get("/panic", func(http.ResponseWriter, *http.Request) { panic("boom") })

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/panic", http.NoBody))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.InDelta(t, 1, obstest.Value(t, api.metrics, "uptime_http_requests_total",
		"method", "GET", "route", "/panic", "code", "500"), 0)
}

// The metrics middleware must not leak label values from the path into the
// series names either: a sanity check on the exposition.
func TestMetricsExpositionHasNoIDs(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, fakeDB{}, slog.New(slog.DiscardHandler))
	id := uuid.NewString()
	api.do(t, http.MethodGet, "/api/v1/monitors/"+id, "", api.userToken(t, "bob@example.com"))

	rec := httptest.NewRecorder()
	observability.Handler(api.metrics).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, strings.Contains(rec.Body.String(), `route="/api/v1/monitors/{id}"`))
	assert.NotContains(t, rec.Body.String(), id)
}
