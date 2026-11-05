// Command uptime-monitor contains the api, worker and migrate commands in one
// binary. Started through a link named api, worker or migrate it runs that
// command; otherwise the first argument names it:
//
//	/app/worker -healthcheck
//	uptime-monitor migrate up
//
// The Docker image ships this binary: the three commands share almost all
// their code, so one binary is about half the size of three.
package main

import (
	"fmt"
	"os"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/app"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/app/api"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/app/migrate"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/app/worker"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
)

// Build metadata, set by the linker (see LDFLAGS in the Makefile).
var (
	version = "dev"
	commit  = "unknown"
	builtAt = "unknown"
)

func main() {
	commands := map[string]app.RunFunc{
		"api":     api.Run,
		"worker":  worker.Run,
		"migrate": migrate.Run,
	}
	name, args, err := app.Select(os.Args[0], os.Args[1:], commands)
	if err != nil {
		// Usage error: no logger is involved yet, and 2 is the usual exit
		// status for wrong usage.
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	app.Main(name, commands[name], observability.NewBuildInfo(version, commit, builtAt), args)
}
