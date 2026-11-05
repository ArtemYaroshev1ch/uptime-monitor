// Command api serves the public REST API of Uptime Monitor.
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

	"github.com/artemyaroshev1ch/uptime-monitor/internal/auth"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/checker"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/config"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/httpapi"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/httpapi/middleware"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/notifier"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/clock"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/healthcheck"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/httpserver"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/logger"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/ops"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/postgres"
	repository "github.com/artemyaroshev1ch/uptime-monitor/internal/repository/postgres"
	svc "github.com/artemyaroshev1ch/uptime-monitor/internal/service"
)

// Build metadata, set by the linker (see LDFLAGS in the Makefile):
//
//	go build -ldflags "-X main.version=1.2.3 -X main.commit=abc -X main.builtAt=..."
var (
	version = "dev"
	commit  = "unknown"
	builtAt = "unknown"
)

const service = "api"

// healthcheckTimeout is how long -healthcheck waits for /healthz.
const healthcheckTimeout = 3 * time.Second

// opsWriteTimeout lets a CPU profile of up to a minute be taken on the ops
// port (see httpserver.WithWriteTimeout).
const opsWriteTimeout = 70 * time.Second

// authRateLimit is how many /api/v1/auth/* requests one IP may make per minute.
const authRateLimit = 10

func main() {
	info := observability.NewBuildInfo(version, commit, builtAt)
	if err := run(context.Background(), os.Args[1:], info, os.Stdout); err != nil {
		// The configured logger may not exist yet (e.g. config is invalid), so
		// the error is reported in the production format.
		log := logger.New(os.Stdout, logger.Options{
			Format:  logger.FormatJSON,
			Level:   slog.LevelInfo,
			Service: service,
			Version: info.Version,
		})
		log.Error("api failed", slog.Any("error", err))
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

	cfg, err := config.LoadAPI()
	if err != nil {
		return err
	}

	if *probe {
		return runHealthcheck(ctx, cfg.HTTPAddr)
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

func serve(ctx context.Context, cfg config.API, info observability.BuildInfo, stdout io.Writer) error {
	log := logger.New(stdout, logger.Options{
		Format:  logFormat(cfg.Env),
		Level:   cfg.LogLevel,
		Service: service,
		Version: info.Version,
	})

	// SIGTERM is what Docker and Kubernetes send on stop; os.Interrupt is
	// Ctrl+C. Either one cancels ctx, which starts the graceful shutdown.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	opsLn, err := lc.Listen(ctx, "tcp", cfg.OpsAddr)
	if err != nil {
		return errors.Join(fmt.Errorf("listen ops: %w", err), ln.Close())
	}

	log.Info("api starting",
		slog.String("env", string(cfg.Env)),
		slog.String("commit", info.Commit),
		slog.String("go_version", info.GoVersion),
	)
	if cfg.PprofEnabled {
		log.Warn("PPROF_ENABLED is on: the profiler is served on the ops port")
	}

	pool, err := postgres.NewPool(ctx, postgres.PoolConfig{URL: cfg.DB.URL, MaxConns: cfg.DB.MaxConns})
	if err != nil {
		return errors.Join(err, ln.Close(), opsLn.Close())
	}
	// Deferred, so it runs after both servers have returned: in-flight
	// requests have finished with their connections by then.
	defer pool.Close()

	reg := observability.NewRegistry(info)
	reg.MustRegister(observability.NewPoolCollector(pool))

	clk := clock.System{}
	tokens := auth.NewTokens([]byte(cfg.Auth.JWTSecret), cfg.Auth.AccessTokenTTL, clk)
	authSvc, err := svc.NewAuth(repository.NewUsers(pool), auth.NewPasswords(auth.DefaultBcryptCost), tokens)
	if err != nil {
		return errors.Join(err, ln.Close(), opsLn.Close())
	}
	if cfg.AllowPrivateTargets {
		log.Warn("ALLOW_PRIVATE_TARGETS is on: monitors may target internal addresses; never use this in production")
	}
	monitorRepo := repository.NewMonitors(pool)
	monitorSvc := svc.NewMonitors(monitorRepo, checker.NewTargetPolicy(cfg.AllowPrivateTargets), clk, cfg.MaxMonitorsPerUser)
	checkSvc := svc.NewChecks(monitorRepo, repository.NewChecks(pool), clk)
	incidentSvc := svc.NewIncidents(repository.NewIncidents(pool))
	sender := notifier.NewDispatcher(notifier.DispatcherOptions{
		TelegramToken: cfg.Notify.TelegramBotToken,
		AllowPrivate:  cfg.AllowPrivateTargets,
		UserAgent:     "uptime-monitor/" + info.Version + " (+https://github.com/artemyaroshev1ch/uptime-monitor)",
	})
	channelSvc := svc.NewChannels(repository.NewChannels(pool), checker.NewTargetPolicy(cfg.AllowPrivateTargets), sender, clk)

	router := httpapi.NewRouter(httpapi.Deps{
		BuildInfo:         info,
		DB:                pool,
		Log:               log,
		Clock:             clk,
		Metrics:           observability.NewHTTPMetrics(reg),
		Auth:              authSvc,
		Monitors:          monitorSvc,
		Checks:            checkSvc,
		Incidents:         incidentSvc,
		Channels:          channelSvc,
		Tokens:            tokens,
		AuthLimiter:       middleware.NewRateLimiter(authRateLimit, time.Minute, clk),
		TrustProxyHeaders: cfg.TrustProxyHeaders,
	})
	opsHandler := ops.Handler(log, ops.Options{
		Checks:  []ops.Check{{Name: "database", Fn: pool.Ping}},
		Metrics: observability.Handler(reg),
		Pprof:   cfg.PprofEnabled,
	})

	// errgroup runs both servers with a shared context: on SIGTERM (or if
	// one of them fails) both shut down gracefully; Wait returns the first
	// error.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return httpserver.New("public", router, log, cfg.ShutdownTimeout).Serve(gctx, ln) })
	g.Go(func() error {
		return httpserver.New("ops", opsHandler, log, cfg.ShutdownTimeout, httpserver.WithWriteTimeout(opsWriteTimeout)).
			Serve(gctx, opsLn)
	})
	if err := g.Wait(); err != nil {
		return err
	}

	log.Info("api stopped")
	return nil
}

func logFormat(env config.Env) logger.Format {
	if env == config.EnvProd {
		return logger.FormatJSON
	}
	return logger.FormatText
}
