// Package notifier delivers notifications: the Telegram and webhook senders
// and the outbox loop of the worker that drives them.
package notifier

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/clock"
)

// Sender delivers one message to one channel. Errors marked with Permanent
// are not retried. Error texts are stored and shown to the user, so they
// must never contain secrets.
type Sender interface {
	Send(ctx context.Context, ch domain.Channel, msg domain.Message) error
}

// Dispatcher picks the sender of a channel's type.
type Dispatcher map[domain.ChannelType]Sender

// DispatcherOptions configures NewDispatcher.
type DispatcherOptions struct {
	// TelegramToken is TELEGRAM_BOT_TOKEN; empty leaves Telegram out.
	TelegramToken string
	// AllowPrivate (ALLOW_PRIVATE_TARGETS) lets webhooks reach internal
	// addresses.
	AllowPrivate bool
	// UserAgent is sent with webhook deliveries.
	UserAgent string
}

// NewDispatcher returns the senders of every configured channel type.
func NewDispatcher(opts DispatcherOptions) Dispatcher {
	d := Dispatcher{
		domain.ChannelWebhook: NewWebhook(WebhookOptions{AllowPrivate: opts.AllowPrivate, UserAgent: opts.UserAgent}),
	}
	if opts.TelegramToken != "" {
		d[domain.ChannelTelegram] = NewTelegram(TelegramOptions{Token: opts.TelegramToken})
	}
	return d
}

// Send delivers msg through the sender of ch.Type. A type without a sender
// (Telegram without TELEGRAM_BOT_TOKEN) is a permanent failure.
func (d Dispatcher) Send(ctx context.Context, ch domain.Channel, msg domain.Message) error {
	s, ok := d[ch.Type]
	if !ok || s == nil {
		return Permanent(fmt.Errorf("%s notifications are not configured on this server", ch.Type))
	}
	return s.Send(ctx, ch, msg)
}

// Store is the outbox (repository/postgres.Notifications).
type Store interface {
	// Claim takes up to limit due pending notifications and hides them
	// from other claimers for lease.
	Claim(ctx context.Context, limit int, lease time.Duration) ([]domain.Notification, error)
	MarkSent(ctx context.Context, id int64, at time.Time) error
	MarkRetry(ctx context.Context, id int64, next time.Time, lastErr string) error
	MarkFailed(ctx context.Context, id int64, lastErr string) error
}

// Config tunes the outbox loop.
type Config struct {
	// Interval is how often due notifications are looked for.
	Interval time.Duration
	// Batch is the most notifications claimed (and sent in parallel) per
	// tick.
	Batch int
	// SendTimeout bounds one delivery.
	SendTimeout time.Duration
	// Lease is how long a claimed notification is hidden from other
	// workers. It must exceed SendTimeout plus the time to record the
	// outcome; if a worker dies mid-send, the notification is retried
	// once the lease ends.
	Lease time.Duration
	// MaxAttempts is the number of deliveries tried before a notification
	// is failed.
	MaxAttempts int
	// BaseBackoff is the wait after the first failure; it doubles after
	// every further failure, up to MaxBackoff.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

// DefaultConfig is the configuration of the worker: five attempts spread
// over about eight minutes (30 s, 1 m, 2 m, 4 m between them).
func DefaultConfig() Config {
	return Config{
		Interval:    time.Second,
		Batch:       10,
		SendTimeout: 10 * time.Second,
		Lease:       time.Minute,
		MaxAttempts: 5,
		BaseBackoff: 30 * time.Second,
		MaxBackoff:  15 * time.Minute,
	}
}

// recordTimeout bounds writing the outcome of one delivery.
const recordTimeout = 5 * time.Second

// Notifier is the outbox loop of the worker.
type Notifier struct {
	store  Store
	sender Sender
	clock  clock.Clock
	log    *slog.Logger
	cfg    Config
}

// New returns a Notifier.
func New(store Store, sender Sender, clk clock.Clock, log *slog.Logger, cfg Config) *Notifier {
	return &Notifier{store: store, sender: sender, clock: clk, log: log.With(slog.String("component", "notifier")), cfg: cfg}
}

// Backoff is the wait after the attempt-th failed attempt (1-based):
// base, 2*base, 4*base, ... capped at maxWait.
func Backoff(attempt int, base, maxWait time.Duration) time.Duration {
	d := base
	for i := 1; i < attempt && d < maxWait; i++ {
		d *= 2
	}
	return min(d, maxWait)
}

// Run delivers due notifications every Interval until ctx is cancelled.
// Deliveries in progress when ctx is cancelled are finished (each is
// bounded by SendTimeout), so a shutdown does not cause duplicate sends.
func (n *Notifier) Run(ctx context.Context) error {
	ticker := time.NewTicker(n.cfg.Interval)
	defer ticker.Stop()
	for {
		// A full batch means there is a backlog: go on without waiting for
		// the tick.
		if n.RunOnce(ctx) == n.cfg.Batch && ctx.Err() == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce claims one batch, delivers it in parallel and records the
// outcomes. It returns the number of notifications claimed.
func (n *Notifier) RunOnce(ctx context.Context) int {
	if ctx.Err() != nil {
		return 0
	}
	batch, err := n.store.Claim(ctx, n.cfg.Batch, n.cfg.Lease)
	if err != nil {
		if ctx.Err() == nil {
			n.log.ErrorContext(ctx, "claim notifications", slog.Any("error", err))
		}
		return 0
	}

	// The deliveries must not be cut off by shutdown: an interrupted
	// request may still have reached the receiver, and the notification
	// would then be sent again after the lease. WithoutCancel keeps ctx's
	// values (logging attributes) but not its cancellation; the timeouts
	// bound the work instead.
	work := context.WithoutCancel(ctx)
	var wg sync.WaitGroup
	for _, nt := range batch {
		wg.Go(func() { n.deliver(work, nt) })
	}
	wg.Wait()
	return len(batch)
}

// deliver sends one notification and records the outcome.
func (n *Notifier) deliver(ctx context.Context, nt domain.Notification) {
	log := n.log.With(
		slog.Int64("notification_id", nt.ID),
		slog.String("event", string(nt.Message.Event)),
		slog.String("channel_type", string(nt.Channel.Type)),
		slog.String("monitor_id", nt.Message.Monitor.ID.String()),
	)
	if nt.Message.Incident != nil {
		log = log.With(slog.String("incident_id", nt.Message.Incident.ID.String()))
	}
	attempt := nt.Attempts + 1

	var err error
	if !nt.Channel.IsActive {
		err = Permanent(fmt.Errorf("channel is disabled"))
	} else {
		sendCtx, cancel := context.WithTimeout(ctx, n.cfg.SendTimeout)
		nt.Message.At = n.clock.Now()
		err = n.sender.Send(sendCtx, nt.Channel, nt.Message)
		cancel()
	}

	recCtx, cancel := context.WithTimeout(ctx, recordTimeout)
	defer cancel()
	now := n.clock.Now()
	switch {
	case err == nil:
		if rerr := n.store.MarkSent(recCtx, nt.ID, now); rerr != nil {
			log.ErrorContext(ctx, "record sent notification", slog.Any("error", rerr))
			return
		}
		log.InfoContext(ctx, "notification sent",
			slog.Int("attempt", attempt),
			slog.Duration("delay", now.Sub(nt.CreatedAt)))
	case IsPermanent(err) || attempt >= n.cfg.MaxAttempts:
		if rerr := n.store.MarkFailed(recCtx, nt.ID, err.Error()); rerr != nil {
			log.ErrorContext(ctx, "record failed notification", slog.Any("error", rerr))
			return
		}
		log.WarnContext(ctx, "notification failed",
			slog.Int("attempt", attempt),
			slog.Bool("permanent", IsPermanent(err)),
			slog.Any("error", err))
	default:
		next := now.Add(Backoff(attempt, n.cfg.BaseBackoff, n.cfg.MaxBackoff))
		if rerr := n.store.MarkRetry(recCtx, nt.ID, next, err.Error()); rerr != nil {
			log.ErrorContext(ctx, "record notification retry", slog.Any("error", rerr))
			return
		}
		log.WarnContext(ctx, "notification will be retried",
			slog.Int("attempt", attempt),
			slog.Time("next_attempt_at", next),
			slog.Any("error", err))
	}
}
