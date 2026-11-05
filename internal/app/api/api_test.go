package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
)

// These tests use t.Setenv, which forbids t.Parallel: the environment is
// process-wide.

var testInfo = observability.BuildInfo{
	Version:   "1.2.3",
	Commit:    "abc123",
	BuiltAt:   "2026-01-02T03:04:05Z",
	GoVersion: "go1.27.0",
}

// setEnv isolates a test from variables set on the developer's machine.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	defaults := map[string]string{
		"APP_ENV":          "prod",
		"LOG_LEVEL":        "info",
		"HTTP_ADDR":        "127.0.0.1:0",
		"OPS_ADDR":         "127.0.0.1:0",
		"SHUTDOWN_TIMEOUT": "5s",
		// Nothing listens on port 1. The pool connects lazily, so the server
		// starts anyway; only /readyz would notice.
		"DATABASE_URL": "postgres://uptime:uptime@127.0.0.1:1/uptime",
		"JWT_SECRET":   "0123456789abcdef0123456789abcdef",
	}
	for k, v := range defaults {
		t.Setenv(k, v)
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestRunVersion(t *testing.T) {
	var out strings.Builder

	err := Run(t.Context(), []string{"-version"}, testInfo, &out)

	require.NoError(t, err)
	assert.Equal(t, "api 1.2.3 (commit abc123, built 2026-01-02T03:04:05Z, go1.27.0)\n", out.String())
}

func TestRunInvalidConfig(t *testing.T) {
	setEnv(t, map[string]string{"APP_ENV": "staging"})

	err := Run(t.Context(), nil, testInfo, io.Discard)

	require.ErrorContains(t, err, "APP_ENV")
}

func TestRunHealthcheck(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "healthy", status: http.StatusOK},
		{name: "unhealthy", status: http.StatusServiceUnavailable, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(tt.status)
			}))
			t.Cleanup(srv.Close)
			setEnv(t, map[string]string{"HTTP_ADDR": srv.Listener.Addr().String()})

			err := Run(t.Context(), []string{"-healthcheck"}, testInfo, io.Discard)

			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestRunServesUntilCancelled starts the real servers on free ports, calls
// them and then stops them the way a SIGTERM would: by cancelling ctx.
func TestRunServesUntilCancelled(t *testing.T) {
	setEnv(t, map[string]string{"PPROF_ENABLED": "true"})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	logs, logWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := Run(ctx, nil, testInfo, logWriter)
		_ = logWriter.Close()
		done <- err
	}()

	addrs := waitForListenAddrs(t, logs, "public", "ops")
	go func() { _, _ = io.Copy(io.Discard, logs) }() // keep the pipe flowing
	public, opsPort := "http://"+addrs["public"], "http://"+addrs["ops"]

	code, _ := fetch(t, public+"/healthz")
	assert.Equal(t, http.StatusOK, code)
	code, _ = fetch(t, public+"/version")
	assert.Equal(t, http.StatusOK, code)
	code, _ = fetch(t, public+"/metrics")
	assert.Equal(t, http.StatusNotFound, code, "metrics are never served on the public port")
	code, _ = fetch(t, public+"/debug/pprof/")
	assert.Equal(t, http.StatusNotFound, code, "pprof is never served on the public port")

	code, body := fetch(t, opsPort+"/metrics")
	assert.Equal(t, http.StatusOK, code)
	for _, name := range []string{
		"uptime_build_info", "uptime_http_requests_total", "uptime_http_request_duration_seconds",
		"uptime_http_requests_in_flight", "uptime_db_pool_acquired_connections", "uptime_db_pool_idle_connections",
		"uptime_db_pool_total_connections", "uptime_db_pool_max_connections", "uptime_db_pool_acquire_wait_seconds_total",
		"go_goroutines", "process_cpu_seconds_total",
	} {
		assert.Contains(t, body, "\n# TYPE "+name+" ", "metric %s", name)
	}
	assert.Contains(t, body, `uptime_http_requests_total{code="200",method="GET",route="/version"} 1`)
	assert.Contains(t, body, `uptime_http_requests_total{code="404",method="GET",route="unmatched"} 2`)
	assert.NotContains(t, body, `route="/healthz"`)
	code, _ = fetch(t, opsPort+"/debug/pprof/")
	assert.Equal(t, http.StatusOK, code, "PPROF_ENABLED=true")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
}

// fetch returns the status and body of GET url.
func fetch(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	require.NoError(t, err)
	// No keep-alive, so no spare connection delays the shutdown (see the
	// same client in internal/app/worker/worker_test.go).
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode, string(body)
}

// waitForListenAddrs reads JSON log lines until every named server reports
// its address.
func waitForListenAddrs(t *testing.T, logs io.Reader, servers ...string) map[string]string {
	t.Helper()
	addrs := map[string]string{}
	scanner := bufio.NewScanner(logs)
	for scanner.Scan() {
		var record struct {
			Msg    string `json:"msg"`
			Server string `json:"server"`
			Addr   string `json:"addr"`
		}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &record), "log line: %s", scanner.Text())
		if record.Msg == "http server listening" {
			addrs[record.Server] = record.Addr
		}
		if len(addrs) == len(servers) {
			for _, s := range servers {
				require.Contains(t, addrs, s)
			}
			return addrs
		}
	}
	t.Fatalf("api exited before listening: %v", scanner.Err())
	return nil
}
