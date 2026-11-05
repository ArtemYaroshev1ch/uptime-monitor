package observability

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// PoolStater is a connection pool that reports its state (*pgxpool.Pool).
type PoolStater interface {
	Stat() *pgxpool.Stat
}

// poolCollector exports pgxpool.Stat on every scrape. A custom Collector
// instead of gauges set by a goroutine: the values are read at scrape time,
// so they are never stale and nothing runs between scrapes.
type poolCollector struct {
	pool PoolStater

	acquired, idle, total, maxConns *prometheus.Desc
	acquires, waits, waitSeconds    *prometheus.Desc
}

// NewPoolCollector returns the uptime_db_pool_* metrics of pool.
func NewPoolCollector(pool PoolStater) prometheus.Collector {
	desc := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc(prometheus.BuildFQName(namespace, "db_pool", name), help, nil, nil)
	}
	return &poolCollector{
		pool:     pool,
		acquired: desc("acquired_connections", "Connections currently in use."),
		idle:     desc("idle_connections", "Open connections not in use."),
		total:    desc("total_connections", "Open connections (acquired, idle and being opened)."),
		maxConns: desc("max_connections", "Maximum size of the pool."),
		acquires: desc("acquires_total", "Connections taken from the pool."),
		waits: desc("acquire_waits_total",
			"Acquires that had to wait because no connection was free (pool saturation)."),
		waitSeconds: desc("acquire_wait_seconds_total",
			"Time spent waiting for a free connection, summed over all acquires."),
	}
}

// Describe implements prometheus.Collector.
func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.acquired, c.idle, c.total, c.maxConns, c.acquires, c.waits, c.waitSeconds} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	gauge := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v)
	}
	counter := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v)
	}
	gauge(c.acquired, float64(s.AcquiredConns()))
	gauge(c.idle, float64(s.IdleConns()))
	gauge(c.total, float64(s.TotalConns()))
	gauge(c.maxConns, float64(s.MaxConns()))
	counter(c.acquires, float64(s.AcquireCount()))
	counter(c.waits, float64(s.EmptyAcquireCount()))
	counter(c.waitSeconds, s.EmptyAcquireWaitTime().Seconds())
}
