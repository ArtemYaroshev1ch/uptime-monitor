// Command api runs the api command of Uptime Monitor on its own. The Docker
// image ships the all-in-one binary instead (see cmd/uptime-monitor).
package main

import (
	"os"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/app"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/app/api"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/observability"
)

// Build metadata, set by the linker (see LDFLAGS in the Makefile):
//
//	go build -ldflags "-X main.version=1.2.3 -X main.commit=abc -X main.builtAt=..."
var (
	version = "dev"
	commit  = "unknown"
	builtAt = "unknown"
)

func main() {
	app.Main("api", api.Run, observability.NewBuildInfo(version, commit, builtAt), os.Args[1:])
}
