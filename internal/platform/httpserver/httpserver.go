// Package httpserver runs http.Server instances with production timeouts and
// graceful shutdown. Both the public API and the ops servers use it.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"
)

// Server is an http.Server that shuts down gracefully when its context ends.
type Server struct {
	name            string
	srv             *http.Server
	log             *slog.Logger
	shutdownTimeout time.Duration
}

// Option changes a default of the underlying http.Server.
type Option func(*http.Server)

// WithWriteTimeout sets how long writing a response may take. The ops port
// needs more than the default: a CPU profile (/debug/pprof/profile) records
// for 30 s by default, and net/http/pprof refuses durations that do not fit
// in the write timeout.
func WithWriteTimeout(d time.Duration) Option {
	return func(s *http.Server) { s.WriteTimeout = d }
}

// New returns a Server serving h. The name ("public", "ops") appears in logs.
// shutdownTimeout bounds how long in-flight requests may take to finish.
func New(name string, h http.Handler, log *slog.Logger, shutdownTimeout time.Duration, opts ...Option) *Server {
	log = log.With(slog.String("server", name))
	s := &Server{
		name: name,
		srv: &http.Server{
			Handler: h,
			// Without these timeouts a slow client can hold a connection and a
			// goroutine forever (Slowloris). ReadHeaderTimeout is the important
			// one: it limits how long a client may take to send headers.
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
			// net/http reports some problems (TLS handshake errors, panics in
			// handlers) through a *log.Logger; route them into slog.
			ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelError),
		},
		log:             log,
		shutdownTimeout: shutdownTimeout,
	}
	for _, opt := range opts {
		opt(s.srv)
	}
	return s
}

// Serve accepts connections on ln until ctx is cancelled (e.g. by SIGTERM) or
// the server fails. On cancellation it stops accepting new connections and
// waits up to the shutdown timeout for in-flight requests to complete.
// It returns nil after a clean shutdown.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	// errgroup runs the server and the shutdown watcher side by side. If the
	// server fails, gctx is cancelled, so the watcher wakes up instead of
	// waiting for a signal forever. Wait returns the first error.
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		s.log.Info("http server listening", slog.String("addr", ln.Addr().String()))
		// Serve always returns an error; ErrServerClosed means Shutdown was called.
		if err := s.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("%s server: %w", s.name, err)
		}
		return nil
	})

	g.Go(func() error {
		<-gctx.Done()
		s.log.Info("http server shutting down", slog.Duration("timeout", s.shutdownTimeout))

		// ctx is already cancelled, so the shutdown deadline needs a context of
		// its own. WithoutCancel keeps ctx values but drops its cancellation.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
		defer cancel()

		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			// Requests are still running after the deadline: drop them.
			return errors.Join(fmt.Errorf("%s server: graceful shutdown: %w", s.name, err), s.srv.Close())
		}
		s.log.Info("http server stopped")
		return nil
	})

	return g.Wait()
}
