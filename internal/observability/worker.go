package observability

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
)

// Label values of uptime_checks_total.
const (
	resultSuccess = "success"
	resultFailure = "failure"
	// errorKindNone is the error_kind of successful checks. An empty label
	// value would be the same as no label in PromQL, which is confusing.
	errorKindNone = "none"
)

// Notification outcomes, the status label of uptime_notifications_total.
const (
	NotificationSent   = "sent"
	NotificationRetry  = "retry"
	NotificationFailed = "failed"
)

// SchedulerMetrics are the metrics of the check loop: volume and quality of
// checks, timeliness (the lag SLI) and saturation of the pool.
type SchedulerMetrics struct {
	checks          *prometheus.CounterVec
	checkDuration   prometheus.Histogram
	lag             prometheus.Histogram
	poolBusy        prometheus.Gauge
	poolSize        prometheus.Gauge
	incidentsOpened prometheus.Counter
}

// NewSchedulerMetrics registers the scheduler metrics in reg.
func NewSchedulerMetrics(reg prometheus.Registerer) *SchedulerMetrics {
	m := &SchedulerMetrics{
		checks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "checks_total",
			Help:      "Checks performed, by result and error kind (none for successful checks).",
		}, []string{"result", "error_kind"}),
		checkDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "check_duration_seconds",
			Help:      "Time to check a target, including DNS, connect and TLS.",
			Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}),
		lag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "scheduler_lag_seconds",
			Help:      "Delay between when a check was due and when it started (timeliness SLI).",
			Buckets:   []float64{0.5, 1, 2, 5, 10, 30, 60},
		}),
		poolBusy: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "worker_pool_busy",
			Help:      "Checks running right now.",
		}),
		poolSize: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "worker_pool_size",
			Help:      "Checks that may run at once (WORKER_CONCURRENCY).",
		}),
		incidentsOpened: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "incidents_opened_total",
			Help:      "Incidents opened.",
		}),
	}
	// Every label combination exists from the start, so rate() and alerts
	// see 0 instead of a missing series before the first failure of a kind.
	m.checks.WithLabelValues(resultSuccess, errorKindNone)
	for _, k := range domain.ErrorKinds() {
		m.checks.WithLabelValues(resultFailure, string(k))
	}
	reg.MustRegister(m.checks, m.checkDuration, m.lag, m.poolBusy, m.poolSize, m.incidentsOpened)
	return m
}

// SetPoolSize records the size of the check pool.
func (m *SchedulerMetrics) SetPoolSize(n int) { m.poolSize.Set(float64(n)) }

// CheckStarted counts a running check until the returned func is called.
// lag is how late the check started.
func (m *SchedulerMetrics) CheckStarted(lag time.Duration) (done func()) {
	m.lag.Observe(max(lag, 0).Seconds())
	m.poolBusy.Inc()
	return m.poolBusy.Dec
}

// CheckDone records the outcome and duration of a check.
func (m *SchedulerMetrics) CheckDone(res domain.CheckResult, took time.Duration) {
	if res.Success {
		m.checks.WithLabelValues(resultSuccess, errorKindNone).Inc()
	} else {
		kind := res.ErrorKind
		if kind == "" {
			kind = domain.ErrorUnknown
		}
		m.checks.WithLabelValues(resultFailure, string(kind)).Inc()
	}
	m.checkDuration.Observe(took.Seconds())
}

// IncidentOpened counts an opened incident.
func (m *SchedulerMetrics) IncidentOpened() { m.incidentsOpened.Inc() }

// NotifierMetrics are the delivery metrics of the outbox loop.
type NotifierMetrics struct {
	outcomes *prometheus.CounterVec
	delay    *prometheus.HistogramVec
}

// NewNotifierMetrics registers the notifier metrics in reg.
func NewNotifierMetrics(reg prometheus.Registerer) *NotifierMetrics {
	m := &NotifierMetrics{
		outcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "notifications_total",
			Help:      "Delivery attempts by channel type and outcome (sent, retry, failed).",
		}, []string{"channel_type", "status"}),
		delay: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "notification_delivery_delay_seconds",
			Help:      "Time from queueing a notification to its delivery (delivery SLI).",
			Buckets:   []float64{1, 5, 15, 30, 60, 120, 300, 900},
		}, []string{"channel_type"}),
	}
	for _, ct := range domain.ChannelTypes() {
		for _, s := range []string{NotificationSent, NotificationRetry, NotificationFailed} {
			m.outcomes.WithLabelValues(string(ct), s)
		}
		m.delay.WithLabelValues(string(ct))
	}
	reg.MustRegister(m.outcomes, m.delay)
	return m
}

// Sent records a delivered notification and how long it took since it was
// queued.
func (m *NotifierMetrics) Sent(ct domain.ChannelType, delay time.Duration) {
	m.outcomes.WithLabelValues(string(ct), NotificationSent).Inc()
	m.delay.WithLabelValues(string(ct)).Observe(max(delay, 0).Seconds())
}

// Retry records a failed attempt that will be retried.
func (m *NotifierMetrics) Retry(ct domain.ChannelType) {
	m.outcomes.WithLabelValues(string(ct), NotificationRetry).Inc()
}

// Failed records a notification given up on.
func (m *NotifierMetrics) Failed(ct domain.ChannelType) {
	m.outcomes.WithLabelValues(string(ct), NotificationFailed).Inc()
}

// RetentionMetrics show that the cleanup works.
type RetentionMetrics struct {
	deleted     prometheus.Counter
	lastSuccess prometheus.Gauge
}

// NewRetentionMetrics registers the retention metrics in reg.
func NewRetentionMetrics(reg prometheus.Registerer) *RetentionMetrics {
	m := &RetentionMetrics{
		deleted: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "retention_deleted_rows_total",
			Help:      "Check results deleted by the retention loop.",
		}),
		lastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "retention_last_success_timestamp_seconds",
			Help:      "Unix time of the last successful retention run (0 before the first).",
		}),
	}
	reg.MustRegister(m.deleted, m.lastSuccess)
	return m
}

// Deleted counts deleted rows, also those of a run that failed halfway.
func (m *RetentionMetrics) Deleted(n int64) { m.deleted.Add(float64(n)) }

// Succeeded records a successful run at t.
func (m *RetentionMetrics) Succeeded(t time.Time) { m.lastSuccess.Set(float64(t.Unix())) }
