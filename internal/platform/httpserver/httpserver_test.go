package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listen opens a listener on a free loopback port. Loopback also avoids the
// Windows Firewall prompt that binding to all interfaces would cause.
func listen(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	return ln
}

func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

// serve starts s in the background. The returned channel yields Serve's result.
func serve(ctx context.Context, s *Server, ln net.Listener) <-chan error {
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	return done
}

func waitResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
		return nil
	}
}

func TestServerTimeouts(t *testing.T) {
	t.Parallel()

	s := New("test", http.NotFoundHandler(), slog.New(slog.DiscardHandler), time.Second)

	assert.Positive(t, s.srv.ReadHeaderTimeout)
	assert.Positive(t, s.srv.ReadTimeout)
	assert.Positive(t, s.srv.WriteTimeout)
	assert.Positive(t, s.srv.IdleTimeout)
}

func TestWithWriteTimeout(t *testing.T) {
	t.Parallel()

	s := New("test", http.NotFoundHandler(), slog.New(slog.DiscardHandler), time.Second, WithWriteTimeout(time.Minute))

	assert.Equal(t, time.Minute, s.srv.WriteTimeout)
	assert.Positive(t, s.srv.ReadHeaderTimeout, "other defaults are kept")
}

func TestServeAndShutdown(t *testing.T) {
	t.Parallel()

	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "pong")
	})
	s := New("test", h, slog.New(slog.DiscardHandler), time.Second)
	ln := listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := serve(ctx, s, ln)

	resp, err := get(t.Context(), "http://"+ln.Addr().String())
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, err)
	assert.Equal(t, "pong", string(body))

	cancel()
	require.NoError(t, waitResult(t, done))
}

func TestShutdownWaitsForInFlightRequests(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "done")
	})
	s := New("test", h, slog.New(slog.DiscardHandler), 5*time.Second)
	shutdownStarted := make(chan struct{})
	s.srv.RegisterOnShutdown(func() { close(shutdownStarted) })
	ln := listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := serve(ctx, s, ln)

	type result struct {
		status int
		err    error
	}
	reqDone := make(chan result, 1)
	go func() {
		resp, err := get(t.Context(), "http://"+ln.Addr().String())
		if err != nil {
			reqDone <- result{err: err}
			return
		}
		_ = resp.Body.Close()
		reqDone <- result{status: resp.StatusCode}
	}()

	<-entered
	cancel() // simulate SIGTERM while the request is still running
	<-shutdownStarted

	select {
	case err := <-done:
		t.Fatalf("Serve returned before the in-flight request finished: %v", err)
	default:
	}

	close(release)

	res := <-reqDone
	require.NoError(t, res.err)
	assert.Equal(t, http.StatusOK, res.status)
	require.NoError(t, waitResult(t, done))
}

func TestShutdownTimeout(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release // never finishes on its own
	})
	s := New("test", h, slog.New(slog.DiscardHandler), 50*time.Millisecond)
	ln := listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := serve(ctx, s, ln)

	go func() {
		resp, err := get(t.Context(), "http://"+ln.Addr().String())
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	<-entered
	cancel()

	err := waitResult(t, done)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestServeFailure(t *testing.T) {
	t.Parallel()

	s := New("test", http.NotFoundHandler(), slog.New(slog.DiscardHandler), time.Second)
	ln := listen(t)
	require.NoError(t, ln.Close()) // Serve fails immediately on a closed listener

	err := waitResult(t, serve(t.Context(), s, ln))
	require.Error(t, err)
	assert.ErrorContains(t, err, "test server")
}
