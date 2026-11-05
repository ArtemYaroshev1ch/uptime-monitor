package middleware

import (
	"net/http"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/clock"
)

// Metrics records the RED metrics of every request: rate and errors
// (uptime_http_requests_total) and duration. Health probes are left out:
// they are not user traffic, and counting them would dilute the
// availability SLI with requests that always succeed.
//
// The route label is the chi route template, read after the handler has
// run (chi fills it in while routing), or "unmatched". Raw paths contain
// IDs and would create a new series per monitor.
func Metrics(m *observability.HTTPMetrics, clk clock.Clock) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			done := m.InFlight()
			defer done()
			start := clk.Now()
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			if isProbe(r.URL.Path) {
				return
			}
			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			m.Observe(r.Method, routePattern(r), status, clk.Now().Sub(start))
		})
	}
}
