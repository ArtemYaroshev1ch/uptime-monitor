package observability

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Namespace is the prefix of every metric of the service.
const namespace = "uptime"

// NewRegistry returns the registry of one process with the Go runtime
// (go_*), process (process_*) and uptime_build_info metrics. Every component
// registers its metrics here; nothing uses prometheus.DefaultRegisterer, so
// tests get a fresh registry each and never see each other's values.
//
// Registration panics (MustRegister) only on programming errors such as a
// duplicate metric name; the tests that build every registry catch those.
func NewRegistry(info BuildInfo) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		newBuildInfoCollector(info),
	)
	return reg
}

// newBuildInfoCollector is a gauge that is always 1: the labels carry the
// information, so a query can join them to other series ("which version
// produces these errors") or count versions during a rollout.
func newBuildInfoCollector(info BuildInfo) prometheus.Collector {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "build_info",
		Help:      "Build information of the running binary; always 1.",
	}, []string{"version", "commit", "go_version"})
	g.WithLabelValues(info.Version, info.Commit, info.GoVersion).Set(1)
	return g
}

// Handler serves the metrics of reg in the Prometheus text format.
func Handler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
