package certwatch

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/clock"
)

type fakeStore struct {
	mu    sync.Mutex
	calls []time.Time
	res   domain.CertWarnings
	err   error
}

func (s *fakeStore) EnqueueCertExpiring(_ context.Context, now time.Time) (domain.CertWarnings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, now)
	return s.res, s.err
}

func (s *fakeStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// syncBuffer is a bytes.Buffer safe for the logger and the test at once.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunOnce(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		res     domain.CertWarnings
		err     error
		wantLog string
	}{
		{name: "warnings", res: domain.CertWarnings{Monitors: 2, Notifications: 3}, wantLog: "certificate warnings queued"},
		{name: "nothing to do", wantLog: ""},
		{name: "store error", err: errors.New("db down"), wantLog: "db down"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{res: tt.res, err: tt.err}
			var logs syncBuffer
			w := New(store, clock.NewFake(now), slog.New(slog.NewTextHandler(&logs, nil)), time.Hour)

			w.RunOnce(t.Context())

			require.Len(t, store.calls, 1)
			assert.Equal(t, now, store.calls[0], "the injected clock decides what is due")
			if tt.wantLog == "" {
				assert.Empty(t, logs.String())
			} else {
				assert.Contains(t, logs.String(), tt.wantLog)
			}
		})
	}
}

func TestRunChecksAtOnceAndStops(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	w := New(store, clock.System{}, slog.New(slog.NewTextHandler(&syncBuffer{}, nil)), time.Hour)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	require.Eventually(t, func() bool { return store.count() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the first run does not wait for the interval")
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}
