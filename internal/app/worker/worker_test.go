package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
)

// These tests use t.Setenv, which forbids t.Parallel.

var testInfo = observability.BuildInfo{
	Version:   "1.2.3",
	Commit:    "abc123",
	BuiltAt:   "2026-01-02T03:04:05Z",
	GoVersion: "go1.27.0",
}

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	defaults := map[string]string{
		"APP_ENV":          "prod",
		"LOG_LEVEL":        "info",
		"OPS_ADDR":         "127.0.0.1:0",
		"SHUTDOWN_TIMEOUT": "5s",
		// Nothing listens on port 1: the worker starts, but cannot claim
		// work, so it is alive and not ready.
		"DATABASE_URL": "postgres://uptime:uptime@127.0.0.1:1/uptime?connect_timeout=1",
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

	require.NoError(t, Run(t.Context(), []string{"-version"}, testInfo, &out))

	assert.Equal(t, "worker 1.2.3 (commit abc123, built 2026-01-02T03:04:05Z, go1.27.0)\n", out.String())
}

func TestRunInvalidConfig(t *testing.T) {
	setEnv(t, map[string]string{"WORKER_CONCURRENCY": "0"})

	err := Run(t.Context(), nil, testInfo, io.Discard)

	require.ErrorContains(t, err, "WORKER_CONCURRENCY")
}

// TestRunServesUntilCancelled starts the worker without a database: the ops
// port answers liveness, readiness reports the database and the scheduler as
// unavailable, and cancelling ctx (as SIGTERM would) stops it cleanly.
func TestRunServesUntilCancelled(t *testing.T) {
	setEnv(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	logs, logWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := Run(ctx, nil, testInfo, logWriter)
		_ = logWriter.Close()
		done <- err
	}()

	addr := waitForListenAddr(t, logs)
	go func() { _, _ = io.Copy(io.Discard, logs) }()

	assert.Equal(t, http.StatusOK, get(t, "http://"+addr+"/healthz"))
	assert.Equal(t, http.StatusServiceUnavailable, get(t, "http://"+addr+"/readyz"))

	// The worker's metrics, all of them present from the start (the
	// snapshot gauges read 0 until the database answers).
	code, body := fetch(t, "http://"+addr+"/metrics")
	assert.Equal(t, http.StatusOK, code)
	for _, name := range []string{
		"uptime_build_info", "uptime_db_pool_acquired_connections",
		"uptime_checks_total", "uptime_check_duration_seconds", "uptime_scheduler_lag_seconds",
		"uptime_scheduler_due_monitors", "uptime_worker_pool_busy", "uptime_worker_pool_size",
		"uptime_notifications_total", "uptime_notification_delivery_delay_seconds",
		"uptime_notifications_pending", "uptime_notification_oldest_pending_age_seconds",
		"uptime_incidents_open", "uptime_incidents_opened_total", "uptime_monitors",
		"uptime_monitors_cert_expiring", "uptime_retention_deleted_rows_total",
		"uptime_retention_last_success_timestamp_seconds", "go_goroutines", "process_cpu_seconds_total",
	} {
		assert.Contains(t, body, "\n# TYPE "+name+" ", "metric %s", name)
	}
	assert.Contains(t, body, `uptime_build_info{commit="abc123",go_version="go1.27.0",version="1.2.3"} 1`)
	assert.Equal(t, http.StatusNotFound, get(t, "http://"+addr+"/debug/pprof/"), "no pprof on the worker")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
}

func get(t *testing.T, url string) int {
	t.Helper()
	code, _ := fetch(t, url)
	return code
}

// fetch returns the status and body of GET url.
func fetch(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	require.NoError(t, err)
	resp, err := testClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode, string(body)
}

// testClient opens a fresh connection per request and closes it afterwards.
// http.DefaultClient may dial a spare connection that never sends a request;
// http.Server.Shutdown waits up to 5 s for such connections, which made the
// shutdown in TestRunServesUntilCancelled hit its own 5 s timeout under -race.
var testClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

func waitForListenAddr(t *testing.T, logs io.Reader) string {
	t.Helper()
	scanner := bufio.NewScanner(logs)
	for scanner.Scan() {
		var record struct {
			Msg  string `json:"msg"`
			Addr string `json:"addr"`
		}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &record), "log line: %s", scanner.Text())
		if record.Msg == "http server listening" {
			return record.Addr
		}
	}
	t.Fatalf("worker exited before listening: %v", scanner.Err())
	return ""
}
