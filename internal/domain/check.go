package domain

import (
	"time"

	"github.com/google/uuid"
)

// ErrorKind classifies a failed check. The values are a closed list: they
// are stored, returned by the API and used as a metric label.
type ErrorKind string

// Error kinds. An empty ErrorKind means the check succeeded.
const (
	ErrorDNS              ErrorKind = "dns"
	ErrorTimeout          ErrorKind = "timeout"
	ErrorConnection       ErrorKind = "connection"
	ErrorTLS              ErrorKind = "tls"
	ErrorUnexpectedStatus ErrorKind = "unexpected_status"
	ErrorKeywordMismatch  ErrorKind = "keyword_mismatch"
	ErrorBlockedTarget    ErrorKind = "blocked_target"
	ErrorUnknown          ErrorKind = "unknown"
)

// ErrorKinds returns every error kind. Metrics use it to create all label
// values up front, so a kind that has not happened yet reads 0, not "no data".
func ErrorKinds() []ErrorKind {
	return []ErrorKind{
		ErrorDNS, ErrorTimeout, ErrorConnection, ErrorTLS,
		ErrorUnexpectedStatus, ErrorKeywordMismatch, ErrorBlockedTarget, ErrorUnknown,
	}
}

// CheckTarget is what the checker needs to check one monitor. The scheduler
// builds it from a claimed monitor.
type CheckTarget struct {
	MonitorID       uuid.UUID
	URL             string
	Method          string
	Timeout         time.Duration
	ExpectedStatus  *int
	Keyword         *string
	FollowRedirects bool
	// ScheduledFor is when the check was due (next_check_at before the
	// scheduler moved it). Start time minus ScheduledFor is the scheduler
	// lag, the timeliness SLI.
	ScheduledFor time.Time
}

// CheckResult is the outcome of one check. Timings are of the final request
// after redirects; a nil timing means the phase did not happen (no DNS for
// an IP literal, no TLS for http://, nothing after a failed connect).
type CheckResult struct {
	CheckedAt      time.Time
	Success        bool
	StatusCode     *int
	ResponseTimeMs int
	DNSMs          *int
	ConnectMs      *int
	TLSMs          *int
	// TTFBMs is the time from the request being written to the first
	// response byte: how long the server took to answer.
	TTFBMs      *int
	HTTPProto   string // "HTTP/1.1", "HTTP/2.0"; "" without a response
	TLSVersion  string // "TLS1.2", "TLS1.3"; "" for http:// or a failed handshake
	TLSNotAfter *time.Time
	// ErrorKind is "" on success.
	ErrorKind    ErrorKind
	ErrorMessage string
}

// Check is a stored CheckResult.
type Check struct {
	ID        int64
	MonitorID uuid.UUID
	CheckResult
}

// MonitorStats aggregates the checks of one monitor over a period. Latency
// figures cover successful checks only (a timeout says nothing about how
// fast the site is) and are nil when there are none.
type MonitorStats struct {
	TotalChecks   int
	FailedChecks  int
	UptimePercent *float64 // nil without checks
	AvgResponseMs *float64
	P95ResponseMs *float64
	P95TTFBMs     *float64
}
