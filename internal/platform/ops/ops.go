// Package ops builds the handler of an ops port: the internal endpoints that
// orchestrators, Prometheus and operators use. Ops ports are never published
// outside the host network: /metrics and pprof reveal internals, and pprof
// can be made to burn CPU.
package ops

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"
)

// readyTimeout bounds all readiness checks together, below the few seconds
// an orchestrator waits for a probe.
const readyTimeout = 2 * time.Second

// Check is one readiness condition, e.g. "database" or "scheduler".
type Check struct {
	Name string
	// Fn returns nil when the condition holds.
	Fn func(ctx context.Context) error
}

// Options selects what an ops port serves.
type Options struct {
	// Checks decide readiness.
	Checks []Check
	// Metrics serves /metrics; nil leaves it out.
	Metrics http.Handler
	// Pprof serves the Go profiler under /debug/pprof/ (PPROF_ENABLED).
	Pprof bool
}

type readyResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// Handler serves:
//
//	GET /healthz      200 while the process runs; checks nothing (liveness)
//	GET /readyz       200 if every check passes, else 503 (readiness)
//	GET /metrics      Prometheus metrics, if opts.Metrics is set
//	/debug/pprof/...  the Go profiler, if opts.Pprof is set
//
// Readiness responses name the failed checks; the errors go to the log.
func Handler(log *slog.Logger, opts Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()

		res := readyResponse{Status: "ok", Checks: make(map[string]string, len(opts.Checks))}
		code := http.StatusOK
		for _, c := range opts.Checks {
			if err := c.Fn(ctx); err != nil {
				log.WarnContext(ctx, "readiness check failed", slog.String("check", c.Name), slog.Any("error", err))
				res.Checks[c.Name] = "unavailable"
				res.Status = "unavailable"
				code = http.StatusServiceUnavailable
				continue
			}
			res.Checks[c.Name] = "ok"
		}
		writeJSON(w, code, res)
	})
	if opts.Metrics != nil {
		mux.Handle("GET /metrics", opts.Metrics)
	}
	if opts.Pprof {
		// Registered on this mux by hand. Importing net/http/pprof also adds
		// the handlers to http.DefaultServeMux, but no server here serves
		// that one. Index serves the named profiles (heap, goroutine, ...).
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Encoding these fixed types cannot fail; a write error means the
	// client is gone.
	_ = json.NewEncoder(w).Encode(v)
}
