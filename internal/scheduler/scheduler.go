// Package scheduler finds monitors that are due and checks them with a
// bounded pool of goroutines.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/clock"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/logger"
)

// Store claims due monitors and saves results (repository/postgres.Checks).
type Store interface {
	// ClaimDue returns up to limit due monitors and moves them to their next
	// interval, so no other worker gets them.
	ClaimDue(ctx context.Context, limit int) ([]domain.CheckTarget, error)
	// SaveResult stores the result, updates the monitor and opens or
	// resolves an incident. It returns domain.ErrNotFound if the monitor
	// was deleted.
	SaveResult(ctx context.Context, monitorID uuid.UUID, res domain.CheckResult) (domain.IncidentChange, error)
}

// Checker checks one target (checker.Checker).
type Checker interface {
	Check(ctx context.Context, target domain.CheckTarget) domain.CheckResult
}

// Config tunes the scheduler.
type Config struct {
	// Concurrency is the pool size: checks running at once
	// (WORKER_CONCURRENCY).
	Concurrency int
	// BatchSize caps the monitors claimed per tick (SCHEDULER_BATCH_SIZE).
	BatchSize int
	// Tick is how often due monitors are looked for.
	Tick time.Duration
	// ShutdownTimeout is how long running checks may take to finish after
	// the context is cancelled; then they are aborted.
	ShutdownTimeout time.Duration
	// SaveTimeout bounds saving one result.
	SaveTimeout time.Duration
}

// Scheduler is the check loop of the worker.
type Scheduler struct {
	store   Store
	checker Checker
	log     *slog.Logger
	clock   clock.Clock
	cfg     Config
	metrics *observability.SchedulerMetrics

	// slots is a semaphore: a check holds one slot while it runs.
	slots chan struct{}
	// heartbeat is the unix-nano time of the last successful claim. The
	// worker's /readyz fails when it gets stale (the loop is stuck or the
	// database unreachable).
	heartbeat atomic.Int64
}

// New returns a Scheduler.
func New(store Store, checker Checker, metrics *observability.SchedulerMetrics, log *slog.Logger, clk clock.Clock, cfg Config) *Scheduler {
	metrics.SetPoolSize(cfg.Concurrency)
	return &Scheduler{
		store:   store,
		checker: checker,
		log:     log.With(slog.String("component", "scheduler")),
		clock:   clk,
		cfg:     cfg,
		metrics: metrics,
		slots:   make(chan struct{}, cfg.Concurrency),
	}
}

// Heartbeat returns when the loop last claimed work successfully (zero
// before the first time).
func (s *Scheduler) Heartbeat() time.Time {
	ns := s.heartbeat.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// Run claims and checks due monitors every tick until ctx is cancelled. Then
// it stops claiming and waits up to ShutdownTimeout for running checks; any
// still running after that are cancelled. It returns nil after a clean stop.
func (s *Scheduler) Run(ctx context.Context) error {
	s.log.Info("scheduler started",
		slog.Int("concurrency", s.cfg.Concurrency),
		slog.Int("batch_size", s.cfg.BatchSize),
	)

	// Checks run on their own context: shutdown (ctx) must not abort them
	// at once, they get ShutdownTimeout to finish and save their results.
	// WithoutCancel keeps ctx's values (logger attributes) but not its
	// cancellation.
	workCtx, abort := context.WithCancel(context.WithoutCancel(ctx))
	defer abort()
	var running sync.WaitGroup

	ticker := time.NewTicker(s.cfg.Tick)
	defer ticker.Stop()
	for {
		s.tick(ctx, workCtx, &running)
		select {
		case <-ctx.Done():
			s.drain(&running, abort)
			return nil
		case <-ticker.C:
		}
	}
}

// tick claims as many monitors as there are free slots and starts a check
// for each. Claiming more than can run would leave claimed monitors waiting
// in memory: late, and lost if the process stopped.
func (s *Scheduler) tick(ctx, workCtx context.Context, running *sync.WaitGroup) {
	free := cap(s.slots) - len(s.slots)
	if free == 0 {
		// Saturated, but alive: the pool is busy, not stuck.
		s.beat()
		return
	}

	targets, err := s.store.ClaimDue(ctx, min(free, s.cfg.BatchSize))
	if err != nil {
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "claim due monitors", slog.Any("error", err))
		}
		return
	}
	s.beat()

	for _, t := range targets {
		// Never blocks: only this goroutine acquires slots, and at most
		// free of them.
		s.slots <- struct{}{}
		running.Go(func() {
			defer func() { <-s.slots }()
			s.run(workCtx, t)
		})
	}
}

// run checks one target and saves the result.
func (s *Scheduler) run(ctx context.Context, t domain.CheckTarget) {
	ctx = logger.WithAttrs(ctx, slog.String("monitor_id", t.MonitorID.String()))
	start := s.clock.Now()
	// Lag is the timeliness SLI: how late the check starts.
	lag := start.Sub(t.ScheduledFor)
	done := s.metrics.CheckStarted(lag)
	defer done()

	res := s.checker.Check(ctx, t)
	// Counted even if the result cannot be saved: the target was checked.
	s.metrics.CheckDone(res, s.clock.Now().Sub(start))

	saveCtx, cancel := context.WithTimeout(ctx, s.cfg.SaveTimeout)
	defer cancel()
	change, err := s.store.SaveResult(saveCtx, t.MonitorID, res)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			s.log.DebugContext(ctx, "monitor deleted during its check")
			return
		}
		s.log.ErrorContext(ctx, "save check result", slog.Any("error", err))
		return
	}

	s.log.DebugContext(ctx, "check done",
		slog.Bool("success", res.Success),
		slog.String("error_kind", string(res.ErrorKind)),
		slog.Int("response_time_ms", res.ResponseTimeMs),
		slog.Duration("lag", lag),
	)
	if change.Opened != nil {
		s.metrics.IncidentOpened()
	}
	s.logIncident(ctx, change)
}

// logIncident records incident transitions: they are rare and exactly what
// an operator looks for when a user asks "why did I get paged".
func (s *Scheduler) logIncident(ctx context.Context, change domain.IncidentChange) {
	switch {
	case change.Opened != nil:
		s.log.WarnContext(ctx, "incident opened",
			slog.String("incident_id", change.Opened.ID.String()),
			slog.String("cause_kind", string(change.Opened.CauseKind)),
			slog.Time("started_at", change.Opened.StartedAt),
		)
	case change.Resolved != nil:
		inc := change.Resolved
		s.log.InfoContext(ctx, "incident resolved",
			slog.String("incident_id", inc.ID.String()),
			slog.Float64("duration_seconds", inc.ResolvedAt.Sub(inc.StartedAt).Seconds()),
		)
	}
}

// drain waits for running checks, aborting them after ShutdownTimeout.
func (s *Scheduler) drain(running *sync.WaitGroup, abort context.CancelFunc) {
	done := make(chan struct{})
	go func() {
		running.Wait()
		close(done)
	}()

	timer := time.NewTimer(s.cfg.ShutdownTimeout)
	defer timer.Stop()
	select {
	case <-done:
		s.log.Info("scheduler stopped")
	case <-timer.C:
		s.log.Warn("shutdown timeout: aborting running checks", slog.Int("running", len(s.slots)))
		abort()
		<-done
		s.log.Info("scheduler stopped")
	}
}

func (s *Scheduler) beat() {
	s.heartbeat.Store(s.clock.Now().UnixNano())
}
