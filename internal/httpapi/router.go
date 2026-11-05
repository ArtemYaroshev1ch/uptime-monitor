// Package httpapi wires the public HTTP API: routes, middleware and handlers.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/httpapi/handler"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/httpapi/middleware"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/httpapi/response"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/clock"
)

// Deps holds everything the router needs to build handlers.
type Deps struct {
	BuildInfo observability.BuildInfo
	// DB is checked by /readyz.
	DB    handler.Pinger
	Log   *slog.Logger
	Clock clock.Clock
	// Metrics records the HTTP metrics of the public API.
	Metrics *observability.HTTPMetrics

	Auth      handler.AuthService
	Monitors  handler.MonitorService
	Checks    handler.CheckService
	Incidents handler.IncidentService
	Channels  handler.ChannelService
	Tokens    middleware.TokenVerifier
	// AuthLimiter limits /api/v1/auth/* per client IP.
	AuthLimiter *middleware.RateLimiter
	// TrustProxyHeaders: see config.API.TrustProxyHeaders.
	TrustProxyHeaders bool
}

// NewRouter returns the handler of the public API port.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	// Order matters; the first one is the outermost. RequestID comes first,
	// so everything after it has the ID. Metrics wraps the rest, so it times
	// the whole request and sees the final status. Logging sits outside
	// Recover: a panic becomes a 500 in Recover, which Logging and Metrics
	// then record like any other response.
	r.Use(
		middleware.RequestID,
		middleware.Metrics(d.Metrics, d.Clock),
		middleware.Logging(d.Log, d.Clock),
		middleware.Recover(d.Log),
		middleware.BodyLimit(middleware.MaxBodyBytes),
	)
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		response.WriteError(w, http.StatusNotFound, response.CodeNotFound, "Resource not found", nil)
	})

	r.Get("/healthz", handler.Healthz)
	r.Get("/readyz", handler.Readyz(d.DB, d.Log))
	r.Get("/version", handler.Version(d.BuildInfo))

	auth := handler.NewAuth(d.Auth, d.Log)
	monitors := handler.NewMonitors(d.Monitors, d.Log)
	checks := handler.NewChecks(d.Checks, d.Log)
	incidents := handler.NewIncidents(d.Incidents, d.Log)
	channels := handler.NewChannels(d.Channels, d.Log)
	r.Route("/api/v1", func(r chi.Router) {
		// Limits password guessing and mass registration; both routes share
		// one budget per IP. Attached with With rather than in a Route
		// sub-router: With runs after chi has matched the full route, so a
		// rejected request is still logged under "/api/v1/auth/login", not
		// the sub-router's "/api/v1/auth/*".
		authLimit := middleware.RateLimit(d.AuthLimiter, middleware.ClientIP(d.TrustProxyHeaders))
		r.With(authLimit).Post("/auth/register", auth.Register)
		r.With(authLimit).Post("/auth/login", auth.Login)

		r.Group(func(r chi.Router) {
			r.Use(middleware.Authenticate(d.Tokens, d.Log))
			r.Get("/me", auth.Me)

			r.Post("/monitors", monitors.Create)
			r.Get("/monitors", monitors.List)
			r.Get("/monitors/{id}", monitors.Get)
			r.Patch("/monitors/{id}", monitors.Update)
			r.Delete("/monitors/{id}", monitors.Delete)
			r.Get("/monitors/{id}/checks", checks.List)
			r.Get("/monitors/{id}/stats", checks.Stats)

			r.Get("/incidents", incidents.List)
			r.Get("/incidents/{id}", incidents.Get)

			r.Post("/channels", channels.Create)
			r.Get("/channels", channels.List)
			r.Get("/channels/{id}", channels.Get)
			r.Patch("/channels/{id}", channels.Update)
			r.Delete("/channels/{id}", channels.Delete)
			r.Post("/channels/{id}/test", channels.Test)
		})
	})

	return r
}
