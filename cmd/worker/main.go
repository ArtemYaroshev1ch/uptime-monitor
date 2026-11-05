// Command worker checks monitors on schedule, stores the results, delivers
// notifications, warns about expiring certificates and cleans up old checks.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/certwatch"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/checker"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/config"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/notifier"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/clock"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/healthcheck"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/httpserver"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/logger"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/ops"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/postgres"
	repository "github.com/artemyaroshev1ch/uptime-monitor/internal/repository/postgres"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/retention"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/scheduler"
)

// Build metadata, set by the linker (see LDFLAGS in the Makefile).
var (
	version = "dev"
	commit  = "unknown"
	builtAt = "unknown"
)

const service = "worker"

const (
	// healthcheckTimeout is how long -healthcheck waits for /healthz.
	healthcheckTimeout = 3 * time.Second
	// heartbeatMaxAge: /readyz fails when the scheduler has not claimed work
	// for this long (contract with uptime-infra).
	heartbeatMaxAge = 30 * time.Second
	// schedulerTick is how often due monitors are looked for.
	schedulerTick = time.Second
	// saveTimeout bounds saving one check result.
	saveTimeout = 5 * time.Second
	// retentionInterval is how often old checks are deleted.
	retentionInterval = time.Hour
	// retentionBatch is the number of checks deleted per statement.
	retentionBatch = 5000
	// certwatchInterval is how often expiring certificates are looked for.
	certwatchInterval = time.Hour
)

func main() {
	info := observability.NewBuildInfo(version, commit, builtAt)
	if err := run(context.Background(), os.Args[1:], info, os.Stdout); err != nil {
		log := logger.New(os.Stdout, logger.Options{
			Format:  logger.FormatJSON,
			Level:   slog.LevelInfo,
			Service: service,
			Version: info.Version,
		})
		log.Error("worker failed", slog.Any("error", err))
		os.Exit(1)
	}
}

// run is main without os.Exit, so tests can call it. Logs go to stdout.
func run(ctx context.Context, args []string, info observability.BuildInfo, stdout io.Writer) error {
	flags := flag.NewFlagSet(service, flag.ContinueOnError)
	showVersion := flags.Bool("version", false, "print version information and exit")
	probe := flags.Bool("healthcheck", false, "probe /healthz of the running instance and exit (0 = healthy)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if *showVersion {
		_, err := fmt.Fprintf(stdout, "%s %s (commit %s, built %s, %s)\n",
			service, info.Version, info.Commit, info.BuiltAt, info.GoVersion)
		return err
	}

	cfg, err := config.LoadWorker()
	if err != nil {
		return err
	}

	if *probe {
		return runHealthcheck(ctx, cfg.OpsAddr)
	}
	return serve(ctx, cfg, info, stdout)
}

func runHealthcheck(ctx context.Context, listenAddr string) error {
	url, err := healthcheck.URL(listenAddr, "/healthz")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()
	return healthcheck.Check(ctx, url)
}

func serve(ctx context.Context, cfg config.Worker, info observability.BuildInfo, stdout io.Writer) error {
	format := logger.FormatText
	if cfg.Env == config.EnvProd {
		format = logger.FormatJSON
	}
	log := logger.New(stdout, logger.Options{
		Format:  format,
		Level:   cfg.LogLevel,
		Service: service,
		Version: info.Version,
	})

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.OpsAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	log.Info("worker starting",
		slog.String("env", string(cfg.Env)),
		slog.String("commit", info.Commit),
		slog.String("go_version", info.GoVersion),
	)
	if cfg.AllowPrivateTargets {
		log.Warn("ALLOW_PRIVATE_TARGETS is on: checks may reach internal addresses; never use this in production")
	}

	// One connection per running check (to save its result) plus one each
	// for claiming, retention, the notifier and certwatch; a smaller pool
	// would make checks queue for connections.
	maxConns := max(cfg.DB.MaxConns, int32(cfg.Concurrency)+4) //nolint:gosec // Concurrency is validated and small
	pool, err := postgres.NewPool(ctx, postgres.PoolConfig{URL: cfg.DB.URL, MaxConns: maxConns})
	if err != nil {
		return err
	}
	// Runs after the group has finished: no loop uses the pool any more.
	defer pool.Close()

	reg := observability.NewRegistry(info)
	reg.MustRegister(observability.NewPoolCollector(pool))

	userAgent := "uptime-monitor/" + info.Version + " (+https://github.com/artemyaroshev1ch/uptime-monitor)"
	clk := clock.System{}
	checks := repository.NewChecks(pool)
	chk := checker.New(checker.Options{
		AllowPrivate: cfg.AllowPrivateTargets,
		Clock:        clk,
		UserAgent:    userAgent,
	})
	sched := scheduler.New(checks, chk, observability.NewSchedulerMetrics(reg), log, clk, scheduler.Config{
		Concurrency:     cfg.Concurrency,
		BatchSize:       cfg.BatchSize,
		Tick:            schedulerTick,
		ShutdownTimeout: cfg.ShutdownTimeout,
		SaveTimeout:     saveTimeout,
	})
	cleaner := retention.New(checks, observability.NewRetentionMetrics(reg), clk, log, retention.Config{
		Keep:     time.Duration(cfg.RetentionDays) * 24 * time.Hour,
		Interval: retentionInterval,
		Batch:    retentionBatch,
	})
	outbox := repository.NewNotifications(pool)
	sender := notifier.NewDispatcher(notifier.DispatcherOptions{
		TelegramToken: cfg.Notify.TelegramBotToken,
		AllowPrivate:  cfg.AllowPrivateTargets,
		UserAgent:     userAgent,
	})
	if cfg.Notify.TelegramBotToken == "" {
		log.Warn("TELEGRAM_BOT_TOKEN is not set: notifications to telegram channels will fail")
	}
	delivery := notifier.New(outbox, sender, observability.NewNotifierMetrics(reg), clk, log, notifier.DefaultConfig())
	stats := observability.NewStatsCollector(reg, repository.NewStats(pool), clk, log, observability.DefaultStatsConfig())
	watcher := certwatch.New(outbox, clk, log, certwatchInterval)

	opsHandler := ops.Handler(log, ops.Options{
		Checks: []ops.Check{
			{Name: "database", Fn: pool.Ping},
			{Name: "scheduler", Fn: func(context.Context) error {
				if age := clk.Now().Sub(sched.Heartbeat()); age > heartbeatMaxAge {
					return fmt.Errorf("last heartbeat %s ago", age.Round(time.Second))
				}
				return nil
			}},
		},
		Metrics: observability.Handler(reg),
	})

	// errgroup runs the loops side by side with a shared context: when one
	// fails, the context is cancelled and the others stop too; Wait returns
	// the first error. On SIGTERM ctx is cancelled and every loop shuts
	// down gracefully (the scheduler waits for running checks).
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return sched.Run(gctx) })
	g.Go(func() error { return cleaner.Run(gctx) })
	g.Go(func() error { return delivery.Run(gctx) })
	g.Go(func() error { return watcher.Run(gctx) })
	g.Go(func() error { return stats.Run(gctx) })
	g.Go(func() error { return httpserver.New("ops", opsHandler, log, cfg.ShutdownTimeout).Serve(gctx, ln) })
	if err := g.Wait(); err != nil {
		return err
	}

	log.Info("worker stopped")
	return nil
}
