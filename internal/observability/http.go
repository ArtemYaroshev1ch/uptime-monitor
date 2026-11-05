package observability

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// RouteUnmatched is the route label of requests that matched no route.
// Using the raw path instead would let any client create new series by
// requesting random URLs.
const RouteUnmatched = "unmatched"

// MethodOther is the method label of requests with a non-standard method,
// for the same reason.
const MethodOther = "other"

// HTTPMetrics are the RED metrics of the public API: the availability and
// latency SLIs.
type HTTPMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
}

// NewHTTPMetrics registers the HTTP metrics in reg.
func NewHTTPMetrics(reg prometheus.Registerer) *HTTPMetrics {
	m := &HTTPMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "http_requests_total",
			Help:      "HTTP requests handled by the public API, except health probes.",
		}, []string{"method", "route", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "http_request_duration_seconds",
			Help:      "Time to handle an HTTP request of the public API, except health probes.",
			Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}, []string{"method", "route"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "http_requests_in_flight",
			Help:      "HTTP requests of the public API being handled right now.",
		}),
	}
	reg.MustRegister(m.requests, m.duration, m.inFlight)
	return m
}

// InFlight counts a request as in flight until the returned func is called.
func (m *HTTPMetrics) InFlight() (done func()) {
	m.inFlight.Inc()
	return m.inFlight.Dec
}

// Observe records a completed request. route must be a route template or
// RouteUnmatched, never a raw path.
func (m *HTTPMetrics) Observe(method, route string, code int, took time.Duration) {
	method = normalizeMethod(method)
	m.requests.WithLabelValues(method, route, strconv.Itoa(code)).Inc()
	m.duration.WithLabelValues(method, route).Observe(took.Seconds())
}

// normalizeMethod keeps the label to a closed list: the method is client
// input and may be any token.
func normalizeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return method
	default:
		return MethodOther
	}
}
