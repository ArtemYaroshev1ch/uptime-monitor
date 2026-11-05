package domain

import (
	"time"

	"github.com/google/uuid"
)

// ChannelType is how notifications reach a user.
type ChannelType string

// Channel types.
const (
	ChannelTelegram ChannelType = "telegram"
	ChannelWebhook  ChannelType = "webhook"
)

// ChannelTypes returns every channel type.
func ChannelTypes() []ChannelType {
	return []ChannelType{ChannelTelegram, ChannelWebhook}
}

// ChannelConfig is the type-specific part of a channel. Telegram uses ChatID,
// a webhook URL and Secret.
type ChannelConfig struct {
	// ChatID is the Telegram chat the bot writes to (a number or @username).
	ChatID string `json:"chat_id,omitempty"`
	// URL receives webhook deliveries.
	URL string `json:"url,omitempty"`
	// Secret signs webhook deliveries (X-Signature). It is shown to the
	// user once, when the channel is created, and never logged.
	Secret string `json:"secret,omitempty"`
}

// Channel is a destination for a user's notifications.
type Channel struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Type      ChannelType
	Name      string
	Config    ChannelConfig
	IsActive  bool
	CreatedAt time.Time
}

// Event is what a notification reports.
type Event string

// Events. EventTest is never stored: it is sent directly by
// POST /channels/{id}/test.
const (
	EventIncidentOpened   Event = "incident_opened"
	EventIncidentResolved Event = "incident_resolved"
	EventCertExpiring     Event = "cert_expiring"
	EventTest             Event = "test"
)

// NotificationStatus is the delivery state of an outbox row.
type NotificationStatus string

// Notification statuses.
const (
	NotificationPending NotificationStatus = "pending"
	NotificationSent    NotificationStatus = "sent"
	NotificationFailed  NotificationStatus = "failed"
)

// MonitorRef identifies a monitor in a message.
type MonitorRef struct {
	ID   uuid.UUID
	Name string
	URL  string
}

// Message is the content of one notification, independent of the channel
// that carries it. It never contains channel configuration.
type Message struct {
	// ID is the outbox row; 0 for test messages. Deliveries are
	// at-least-once, so receivers can use it to drop duplicates.
	ID      int64
	Event   Event
	Monitor MonitorRef
	// Incident is set for incident events.
	Incident *Incident
	// CertNotAfter is set for cert_expiring: the expiry that was warned
	// about.
	CertNotAfter *time.Time
	// ChannelName is set for test messages.
	ChannelName string
	// At is when the message is sent.
	At time.Time
}

// Notification is a claimed outbox row together with what is needed to
// deliver it.
type Notification struct {
	ID int64
	// Attempts made before this one.
	Attempts  int
	CreatedAt time.Time
	Channel   Channel
	Message   Message
}

// CertWarnings is what one certwatch run did.
type CertWarnings struct {
	// Monitors is the number of monitors warned about.
	Monitors int
	// Notifications is the number of outbox rows created: one per active
	// channel of each monitor's owner.
	Notifications int
}
