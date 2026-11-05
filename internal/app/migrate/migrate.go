// Package migrate is the migrate command: it applies the database migrations
// embedded in the binary.
// Deploys run "migrate up" before starting a new version of api and worker.
package migrate

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/pressly/goose/v3"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/config"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/logger"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/postgres"
)

const service = "migrate"

const usage = `Usage: migrate [flags] <command>

Commands:
  up      apply all pending migrations
  down    roll back the most recently applied migration
  status  list migrations and whether they are applied

Flags:
`

// Run runs the command with args (without the program name) until ctx is
// cancelled or SIGTERM arrives. Logs go to stdout.
func Run(ctx context.Context, args []string, info observability.BuildInfo, stdout io.Writer) error {
	flags := flag.NewFlagSet(service, flag.ContinueOnError)
	showVersion := flags.Bool("version", false, "print version information and exit")
	flags.Usage = func() {
		_, _ = fmt.Fprint(flags.Output(), usage)
		flags.PrintDefaults()
	}
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

	// The command is checked before the configuration, so a typo is reported
	// as such even when DATABASE_URL is not set.
	if flags.NArg() != 1 {
		return fmt.Errorf("expected exactly one command (up, down or status), got %d arguments", flags.NArg())
	}
	command := flags.Arg(0)
	exec, ok := lookupCommand(command)
	if !ok {
		return fmt.Errorf("unknown command %q: want up, down or status", command)
	}

	cfg, err := config.LoadMigrate()
	if err != nil {
		return err
	}
	log := logger.New(stdout, logger.Options{
		Format:  logFormat(cfg.Env),
		Level:   cfg.LogLevel,
		Service: service,
		Version: info.Version,
	})

	// SIGTERM cancels ctx, which cancels the running statement. Each SQL
	// migration runs in a transaction, so PostgreSQL rolls it back and the
	// schema is never left half-migrated.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := postgres.OpenDB(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	migrator, err := postgres.NewMigrator(db)
	if err != nil {
		return err
	}
	return exec(ctx, migrator, log)
}

type command func(ctx context.Context, m *goose.Provider, log *slog.Logger) error

func lookupCommand(name string) (command, bool) {
	switch name {
	case "up":
		return up, true
	case "down":
		return down, true
	case "status":
		return status, true
	}
	return nil, false
}

func up(ctx context.Context, m *goose.Provider, log *slog.Logger) error {
	results, err := m.Up(ctx)
	// Up returns the migrations applied before a failure as well, so the log
	// shows exactly where it stopped.
	for _, r := range results {
		logResult(ctx, log, r)
	}
	if err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}

	current, err := m.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	log.InfoContext(ctx, "migrations up to date",
		slog.Int("applied", len(results)),
		slog.Int64("db_version", current),
	)
	return nil
}

func down(ctx context.Context, m *goose.Provider, log *slog.Logger) error {
	result, err := m.Down(ctx)
	if errors.Is(err, goose.ErrNoNextVersion) {
		log.InfoContext(ctx, "nothing to roll back")
		return nil
	}
	if err != nil {
		return fmt.Errorf("migrate down: %w", err)
	}
	logResult(ctx, log, result)
	return nil
}

func status(ctx context.Context, m *goose.Provider, log *slog.Logger) error {
	statuses, err := m.Status(ctx)
	if err != nil {
		return fmt.Errorf("migrate status: %w", err)
	}
	for _, s := range statuses {
		attrs := []slog.Attr{
			slog.Int64("migration_version", s.Source.Version),
			slog.String("file", s.Source.Path),
			slog.String("state", string(s.State)),
		}
		if !s.AppliedAt.IsZero() {
			attrs = append(attrs, slog.Time("applied_at", s.AppliedAt))
		}
		log.LogAttrs(ctx, slog.LevelInfo, "migration", attrs...)
	}
	return nil
}

func logResult(ctx context.Context, log *slog.Logger, r *goose.MigrationResult) {
	log.InfoContext(ctx, "migration "+r.Direction,
		slog.Int64("migration_version", r.Source.Version),
		slog.String("file", r.Source.Path),
		slog.Duration("duration", r.Duration),
	)
}

func logFormat(env config.Env) logger.Format {
	if env == config.EnvProd {
		return logger.FormatJSON
	}
	return logger.FormatText
}
