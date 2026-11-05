package middleware

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/clock"
)

// isProbe reports whether path is a health probe. Orchestrators and blackbox
// exporters poll those every few seconds; their access logs go to debug
// level, otherwise they would drown everything else.
func isProbe(path string) bool {
	return path == "/healthz" || path == "/readyz"
}

// Logging writes one access log record per request after it completes:
// method, route template, status, size and duration. The request ID comes
// from the context (see RequestID).
func Logging(log *slog.Logger, clk clock.Clock) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := clk.Now()
			// Captures the status code and body size written by the handler.
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			status := ww.Status()
			if status == 0 {
				// The handler wrote nothing; net/http sends 200.
				status = http.StatusOK
			}
			level := slog.LevelInfo
			switch {
			case status >= http.StatusInternalServerError:
				level = slog.LevelError
			case isProbe(r.URL.Path):
				level = slog.LevelDebug
			}

			log.LogAttrs(r.Context(), level, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("route", routePattern(r)),
				slog.Int("status", status),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Float64("duration_ms", float64(clk.Now().Sub(start).Microseconds())/1000),
			)
		})
	}
}

// routePattern returns the chi route template ("/api/v1/monitors/{id}"),
// which groups requests by endpoint, or "unmatched" for unknown paths.
//
// An unknown path under a sub-router (/api/v1/nope) reaches chi's NotFound
// with the sub-router's mount pattern "/api/v1/*". The API registers no
// wildcard routes of its own, so a pattern ending in "/*" always means no
// route matched.
func routePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if p := rctx.RoutePattern(); p != "" && !strings.HasSuffix(p, "/*") {
			return p
		}
	}
	return observability.RouteUnmatched
}
