// Package observability holds what operators use to see inside the service:
// build metadata, the Prometheus registry and the metrics of every component.
//
// The metric names and labels are a public contract (docs/metrics.md): the
// dashboards and alerts in uptime-infra depend on them. Labels take values
// from closed lists only, never user input or IDs, so the number of series
// stays bounded however many users and monitors there are.
package observability

import "runtime"

// BuildInfo describes the running binary. Version, commit and build time are
// injected by the linker (see LDFLAGS in the Makefile) and exposed via
// GET /version, the -version flag and the uptime_build_info metric.
type BuildInfo struct {
	Version   string
	Commit    string
	BuiltAt   string
	GoVersion string
}

// NewBuildInfo returns BuildInfo for the current binary.
func NewBuildInfo(version, commit, builtAt string) BuildInfo {
	return BuildInfo{
		Version:   version,
		Commit:    commit,
		BuiltAt:   builtAt,
		GoVersion: runtime.Version(),
	}
}
