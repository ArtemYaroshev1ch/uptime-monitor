package notifier

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
)

// timeLayout is how times are shown to people: UTC, minute precision.
const timeLayout = "2006-01-02 15:04 UTC"

// Text renders a message as plain text (Telegram). It uses only the
// monitor, incident and certificate: channel configuration, which holds the
// secrets, never reaches it.
func Text(msg domain.Message) string {
	var b strings.Builder
	line := func(format string, args ...any) {
		fmt.Fprintf(&b, format, args...)
		b.WriteByte('\n')
	}

	switch msg.Event {
	case domain.EventIncidentOpened:
		line("🔴 DOWN: %s", msg.Monitor.Name)
		line("%s", msg.Monitor.URL)
		if inc := msg.Incident; inc != nil {
			if inc.CauseKind != "" {
				line("Cause: %s", inc.CauseKind)
			}
			line("Since: %s", inc.StartedAt.UTC().Format(timeLayout))
		}
	case domain.EventIncidentResolved:
		line("✅ UP: %s", msg.Monitor.Name)
		line("%s", msg.Monitor.URL)
		if inc := msg.Incident; inc != nil && inc.ResolvedAt != nil {
			line("Down for %s", inc.ResolvedAt.Sub(inc.StartedAt).Round(time.Second))
			line("Resolved: %s", inc.ResolvedAt.UTC().Format(timeLayout))
		}
	case domain.EventCertExpiring:
		line("⚠️ TLS certificate expiring: %s", msg.Monitor.Name)
		line("%s", msg.Monitor.URL)
		if na := msg.CertNotAfter; na != nil {
			line("Expires: %s (%s)", na.UTC().Format(timeLayout), daysLeft(*na, msg.At))
		}
	case domain.EventTest:
		line("🔔 Test notification from Uptime Monitor")
		line("Channel %q is set up correctly.", msg.ChannelName)
	default:
		line("Uptime Monitor: %s", msg.Event)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func daysLeft(notAfter, now time.Time) string {
	left := notAfter.Sub(now)
	if left <= 0 {
		return "already expired"
	}
	days := int(left.Hours() / 24)
	switch days {
	case 0:
		return "in less than a day"
	case 1:
		return "in 1 day"
	default:
		return fmt.Sprintf("in %d days", days)
	}
}

// Payload is the JSON body of a webhook delivery (documented in
// api/openapi.yaml as WebhookPayload). Like Text, it carries no channel
// configuration.
type Payload struct {
	// ID identifies the notification; deliveries are at-least-once, so a
	// receiver can drop repeats by it. Absent for test deliveries.
	ID          int64               `json:"id,omitempty"`
	Event       domain.Event        `json:"event"`
	SentAt      time.Time           `json:"sent_at"`
	Monitor     *payloadMonitor     `json:"monitor,omitempty"`
	Incident    *payloadIncident    `json:"incident,omitempty"`
	Certificate *payloadCertificate `json:"certificate,omitempty"`
	Channel     *payloadChannel     `json:"channel,omitempty"`
}

type payloadMonitor struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	URL  string    `json:"url"`
}

type payloadIncident struct {
	ID              uuid.UUID  `json:"id"`
	StartedAt       time.Time  `json:"started_at"`
	ResolvedAt      *time.Time `json:"resolved_at"`
	CauseKind       string     `json:"cause_kind,omitempty"`
	DurationSeconds *int64     `json:"duration_seconds,omitempty"`
}

type payloadCertificate struct {
	NotAfter time.Time `json:"not_after"`
}

type payloadChannel struct {
	Name string `json:"name"`
}

// NewPayload builds the webhook body of msg.
func NewPayload(msg domain.Message) Payload {
	p := Payload{ID: msg.ID, Event: msg.Event, SentAt: msg.At.UTC()}
	if msg.Event == domain.EventTest {
		p.Channel = &payloadChannel{Name: msg.ChannelName}
		return p
	}
	p.Monitor = &payloadMonitor{ID: msg.Monitor.ID, Name: msg.Monitor.Name, URL: msg.Monitor.URL}
	if inc := msg.Incident; inc != nil {
		pi := &payloadIncident{ID: inc.ID, StartedAt: inc.StartedAt.UTC(), CauseKind: string(inc.CauseKind)}
		if inc.ResolvedAt != nil {
			at := inc.ResolvedAt.UTC()
			d := int64(at.Sub(inc.StartedAt).Seconds())
			pi.ResolvedAt, pi.DurationSeconds = &at, &d
		}
		p.Incident = pi
	}
	if msg.CertNotAfter != nil {
		p.Certificate = &payloadCertificate{NotAfter: msg.CertNotAfter.UTC()}
	}
	return p
}
