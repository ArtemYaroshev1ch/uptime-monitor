// Package app holds what the binaries share: running a command with
// os.Exit on failure, and choosing the command of the all-in-one binary.
//
// The code of each command lives in internal/app/{api,worker,migrate}. The
// binaries in cmd/ only pass build metadata and call Main. cmd/api,
// cmd/worker and cmd/migrate build one command each (local development);
// cmd/uptime-monitor contains all three and is what the Docker image ships,
// with /app/api, /app/worker and /app/migrate as symbolic links to it. One
// binary instead of three keeps the image small: the commands share almost
// all their dependencies.
package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/logger"
)

// RunFunc runs one command with its arguments (without the program name).
type RunFunc func(ctx context.Context, args []string, info observability.BuildInfo, stdout io.Writer) error

// Main runs a command and exits with status 1 if it fails.
func Main(service string, run RunFunc, info observability.BuildInfo, args []string) {
	if err := run(context.Background(), args, info, os.Stdout); err != nil {
		// The configured logger may not exist yet (e.g. config is invalid), so
		// the error is reported in the production format.
		log := logger.New(os.Stdout, logger.Options{
			Format:  logger.FormatJSON,
			Level:   slog.LevelInfo,
			Service: service,
			Version: info.Version,
		})
		log.Error(service+" failed", slog.Any("error", err))
		os.Exit(1)
	}
}

// Select picks the command of the all-in-one binary. The name it was started
// as decides first, so a symbolic link /app/worker runs the worker;
// otherwise the first argument names the command ("uptime-monitor worker").
// It returns the command name and its arguments.
func Select(argv0 string, args []string, commands map[string]RunFunc) (string, []string, error) {
	name := strings.TrimSuffix(filepath.Base(argv0), ".exe")
	if _, ok := commands[name]; ok {
		return name, args, nil
	}
	if len(args) > 0 {
		if _, ok := commands[args[0]]; ok {
			return args[0], args[1:], nil
		}
	}
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	return "", nil, fmt.Errorf("usage: %s <%s> [flags]", name, strings.Join(names, "|"))
}
