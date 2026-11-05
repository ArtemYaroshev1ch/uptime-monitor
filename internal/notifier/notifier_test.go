package notifier

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability/obstest"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/clock"
)

// fakeStore is an in-memory outbox that honours due times and leases like
// the PostgreSQL one.
type fakeStore struct {
	mu    sync.Mutex
	clock *clock.Fake
	rows  []*fakeRow
}

type fakeRow struct {
	n         domain.Notification
	status    domain.NotificationStatus
	nextAt    time.Time
	lastError string
	sentAt    time.Time
}

func (s *fakeStore) add(n domain.Notification) *fakeRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &fakeRow{n: n, status: domain.NotificationPending, nextAt: s.clock.Now()}
	s.rows = append(s.rows, r)
	return r
}

func (s *fakeStore) Claim(_ context.Context, limit int, lease time.Duration) ([]domain.Notification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Notification
	now := s.clock.Now()
	for _, r := range s.rows {
		if len(out) == limit {
			break
		}
		if r.status == domain.NotificationPending && !r.nextAt.After(now) {
			r.nextAt = now.Add(lease)
			out = append(out, r.n)
		}
	}
	return out, nil
}

func (s *fakeStore) row(id int64) *fakeRow {
	for _, r := range s.rows {
		if r.n.ID == id {
			return r
		}
	}
	panic("unknown notification")
}

func (s *fakeStore) MarkSent(_ context.Context, id int64, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.row(id)
	r.n.Attempts++
	r.status, r.sentAt, r.lastError = domain.NotificationSent, at, ""
	return nil
}

func (s *fakeStore) MarkRetry(_ context.Context, id int64, next time.Time, lastErr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.row(id)
	r.n.Attempts++
	r.nextAt, r.lastError = next, lastErr
	return nil
}

func (s *fakeStore) MarkFailed(_ context.Context, id int64, lastErr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.row(id)
	r.n.Attempts++
	r.status, r.lastError = domain.NotificationFailed, lastErr
	return nil
}

// scriptedSender returns the scripted errors in turn, then nil.
type scriptedSender struct {
	mu    sync.Mutex
	errs  []error
	calls int
}

func (s *scriptedSender) Send(context.Context, domain.Channel, domain.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if len(s.errs) == 0 {
		return nil
	}
	err := s.errs[0]
	s.errs = s.errs[1:]
	return err
}

func newNotifier(sender Sender) (*Notifier, *fakeStore, *clock.Fake) {
	n, store, clk, _ := newNotifierWithMetrics(sender)
	return n, store, clk
}

// newNotifierWithMetrics also returns the registry of the notifier's metrics.
func newNotifierWithMetrics(sender Sender) (*Notifier, *fakeStore, *clock.Fake, *prometheus.Registry) {
	clk := clock.NewFake(started)
	store := &fakeStore{clock: clk}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := prometheus.NewRegistry()
	return New(store, sender, observability.NewNotifierMetrics(reg), clk, log, DefaultConfig()), store, clk, reg
}

func notification(id int64) domain.Notification {
	return domain.Notification{
		ID:        id,
		CreatedAt: started,
		Channel:   webhookChannel("https://hooks.example/"),
		Message:   openedMessage(),
	}
}

func TestBackoff(t *testing.T) {
	t.Parallel()
	base, maxWait := 30*time.Second, 15*time.Minute
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 30 * time.Second},
		{2, time.Minute},
		{3, 2 * time.Minute},
		{4, 4 * time.Minute},
		{5, 8 * time.Minute},
		{6, 15 * time.Minute},
		{60, 15 * time.Minute},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, Backoff(tt.attempt, base, maxWait), "attempt %d", tt.attempt)
	}
}

func TestDeliverSuccess(t *testing.T) {
	t.Parallel()
	n, store, clk, reg := newNotifierWithMetrics(&scriptedSender{})
	r := store.add(notification(1))
	clk.Advance(3 * time.Second)

	assert.Equal(t, 1, n.RunOnce(t.Context()))

	assert.Equal(t, domain.NotificationSent, r.status)
	assert.Equal(t, 1, r.n.Attempts)
	assert.Equal(t, clk.Now(), r.sentAt)
	assert.InDelta(t, 1, obstest.Value(t, reg, "uptime_notifications_total", "channel_type", "webhook", "status", "sent"), 0)
	assert.InDelta(t, 1, obstest.Value(t, reg, "uptime_notification_delivery_delay_seconds", "channel_type", "webhook"), 0)
	assert.InDelta(t, 3, obstest.Sum(t, reg, "uptime_notification_delivery_delay_seconds", "channel_type", "webhook"), 0,
		"delay = sent_at - created_at")
}

// TestRetriesThenFails walks a notification through every attempt: four
// retries with growing backoff, then failed after the fifth.
func TestRetriesThenFails(t *testing.T) {
	t.Parallel()
	failure := errors.New("webhook responded 503 Service Unavailable")
	sender := &scriptedSender{errs: []error{failure, failure, failure, failure, failure, failure}}
	n, store, clk, reg := newNotifierWithMetrics(sender)
	r := store.add(notification(1))
	t.Cleanup(func() {
		outcome := func(status string) float64 {
			return obstest.Value(t, reg, "uptime_notifications_total", "channel_type", "webhook", "status", status)
		}
		assert.InDelta(t, 4, outcome("retry"), 0)
		assert.InDelta(t, 1, outcome("failed"), 0)
		assert.InDelta(t, 0, outcome("sent"), 0)
		assert.InDelta(t, 0, obstest.Value(t, reg, "uptime_notification_delivery_delay_seconds", "channel_type", "webhook"), 0)
	})

	for attempt := 1; attempt <= 4; attempt++ {
		require.Equal(t, 1, n.RunOnce(t.Context()), "attempt %d", attempt)
		assert.Equal(t, domain.NotificationPending, r.status)
		assert.Equal(t, attempt, r.n.Attempts)
		wait := Backoff(attempt, 30*time.Second, 15*time.Minute)
		assert.Equal(t, clk.Now().Add(wait), r.nextAt)
		assert.Equal(t, failure.Error(), r.lastError)

		// Not due before the backoff has passed.
		clk.Advance(wait - time.Second)
		assert.Zero(t, n.RunOnce(t.Context()), "attempt %d: early", attempt)
		clk.Advance(time.Second)
	}

	require.Equal(t, 1, n.RunOnce(t.Context()))
	assert.Equal(t, domain.NotificationFailed, r.status)
	assert.Equal(t, 5, r.n.Attempts)
	assert.Equal(t, 5, sender.calls)

	clk.Advance(time.Hour)
	assert.Zero(t, n.RunOnce(t.Context()), "a failed notification is never claimed again")
}

func TestRetryThenSuccess(t *testing.T) {
	t.Parallel()
	n, store, clk := newNotifier(&scriptedSender{errs: []error{errors.New("connection reset")}})
	r := store.add(notification(1))

	n.RunOnce(t.Context())
	clk.Advance(30 * time.Second)
	n.RunOnce(t.Context())

	assert.Equal(t, domain.NotificationSent, r.status)
	assert.Equal(t, 2, r.n.Attempts)
	assert.Empty(t, r.lastError)
}

func TestPermanentErrorFailsAtOnce(t *testing.T) {
	t.Parallel()
	n, store, _ := newNotifier(&scriptedSender{errs: []error{Permanent(errors.New("chat not found"))}})
	r := store.add(notification(1))

	n.RunOnce(t.Context())

	assert.Equal(t, domain.NotificationFailed, r.status)
	assert.Equal(t, 1, r.n.Attempts)
	assert.Equal(t, "chat not found", r.lastError)
}

func TestDisabledChannelFails(t *testing.T) {
	t.Parallel()
	sender := &scriptedSender{}
	n, store, _ := newNotifier(sender)
	nt := notification(1)
	nt.Channel.IsActive = false
	r := store.add(nt)

	n.RunOnce(t.Context())

	assert.Equal(t, domain.NotificationFailed, r.status)
	assert.Equal(t, "channel is disabled", r.lastError)
	assert.Zero(t, sender.calls)
}

func TestRunOnceBatchesInParallel(t *testing.T) {
	t.Parallel()
	n, store, _ := newNotifier(&scriptedSender{})
	for i := range 15 {
		store.add(notification(int64(i + 1)))
	}

	assert.Equal(t, 10, n.RunOnce(t.Context()), "one batch")
	assert.Equal(t, 5, n.RunOnce(t.Context()), "the rest")
	for _, r := range store.rows {
		assert.Equal(t, domain.NotificationSent, r.status)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	t.Parallel()
	n, store, _ := newNotifier(&scriptedSender{})
	r := store.add(notification(1))
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return r.status == domain.NotificationSent
	}, 5*time.Second, 10*time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestText(t *testing.T) {
	t.Parallel()
	notAfter := started.Add(9*24*time.Hour + time.Hour)
	monitor := domain.MonitorRef{ID: uuid.New(), Name: "Shop", URL: "https://shop.example/"}
	tests := []struct {
		name string
		msg  domain.Message
		want string
	}{
		{
			name: "opened",
			msg:  openedMessage(),
			want: "🔴 DOWN: Shop\nhttps://shop.example/\nCause: timeout\nSince: 2026-11-04 10:00 UTC",
		},
		{
			name: "resolved",
			msg: domain.Message{Event: domain.EventIncidentResolved, Monitor: monitor,
				Incident: &domain.Incident{StartedAt: started, ResolvedAt: &resolved}},
			want: "✅ UP: Shop\nhttps://shop.example/\nDown for 5m30s\nResolved: 2026-11-04 10:05 UTC",
		},
		{
			name: "cert expiring",
			msg:  domain.Message{Event: domain.EventCertExpiring, Monitor: monitor, CertNotAfter: &notAfter, At: started},
			want: "⚠️ TLS certificate expiring: Shop\nhttps://shop.example/\nExpires: 2026-11-13 11:00 UTC (in 9 days)",
		},
		{
			name: "cert expired",
			msg:  domain.Message{Event: domain.EventCertExpiring, Monitor: monitor, CertNotAfter: &started, At: notAfter},
			want: "⚠️ TLS certificate expiring: Shop\nhttps://shop.example/\nExpires: 2026-11-04 10:00 UTC (already expired)",
		},
		{
			name: "test",
			msg:  domain.Message{Event: domain.EventTest, ChannelName: "ops"},
			want: "🔔 Test notification from Uptime Monitor\nChannel \"ops\" is set up correctly.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, Text(tt.msg))
		})
	}
}

func TestPayloadCertAndTest(t *testing.T) {
	t.Parallel()
	notAfter := started.Add(48 * time.Hour)
	p := NewPayload(domain.Message{ID: 7, Event: domain.EventCertExpiring, CertNotAfter: &notAfter, At: started,
		Monitor: domain.MonitorRef{Name: "Shop", URL: "https://shop.example/"}})
	require.NotNil(t, p.Certificate)
	assert.Equal(t, notAfter, p.Certificate.NotAfter)
	assert.Nil(t, p.Incident)

	p = NewPayload(domain.Message{Event: domain.EventTest, ChannelName: "ops", At: started})
	assert.Nil(t, p.Monitor)
	assert.Zero(t, p.ID)
	require.NotNil(t, p.Channel)
	assert.Equal(t, "ops", p.Channel.Name)
}
