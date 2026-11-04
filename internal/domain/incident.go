package domain

import (
	"time"

	"github.com/google/uuid"
)

// Incident is a period during which a monitor was down.
type Incident struct {
	ID          uuid.UUID
	MonitorID   uuid.UUID
	MonitorName string // filled by queries that join monitors
	// StartedAt is the time of the first failed check of the streak that
	// opened the incident, not of the check that reached the threshold:
	// the site was down from the first failure on.
	StartedAt time.Time
	// ResolvedAt is the time of the first successful check afterwards; nil
	// while the incident is open.
	ResolvedAt *time.Time
	// CauseKind is the ErrorKind of the check that opened the incident.
	CauseKind ErrorKind
}

// Open reports whether the incident is still open.
func (i Incident) Open() bool { return i.ResolvedAt == nil }

// IncidentFilter narrows an incident list. Nil fields match everything.
type IncidentFilter struct {
	MonitorID *uuid.UUID
	Open      *bool
}

// IncidentChange reports what saving a check result did to incidents. At
// most one of the fields is set. Its notifications are queued in the same
// transaction.
type IncidentChange struct {
	Opened   *Incident
	Resolved *Incident
}

// Health is the part of a monitor's state that check results change.
type Health struct {
	Status              MonitorStatus
	ConsecutiveFailures int
	// IncidentOpen reports whether the monitor has an open incident.
	IncidentOpen bool
}

// Transition is the outcome of evaluating one check result.
type Transition struct {
	Next Health
	// OpenIncident: this result opens an incident.
	OpenIncident bool
	// ResolveIncident: this result resolves the open incident.
	ResolveIncident bool
}

// Evaluate applies one check result to a monitor's health. It is the whole
// incident policy, kept free of I/O so the boundaries can be tested in
// isolation:
//
//   - success: the failure streak resets, status becomes up, and an open
//     incident is resolved;
//   - failure: the streak grows; once it reaches threshold the status
//     becomes down and, unless one is open already, an incident opens;
//     below the threshold the status stays as it was, so a single blip
//     neither flips the monitor nor pages anyone.
//
// A threshold below 1 is treated as 1.
func Evaluate(h Health, success bool, threshold int) Transition {
	threshold = max(threshold, 1)

	if success {
		return Transition{
			Next:            Health{Status: StatusUp},
			ResolveIncident: h.IncidentOpen,
		}
	}

	next := Health{
		Status:              h.Status,
		ConsecutiveFailures: h.ConsecutiveFailures + 1,
		IncidentOpen:        h.IncidentOpen,
	}
	if next.ConsecutiveFailures < threshold {
		return Transition{Next: next}
	}
	next.Status = StatusDown
	open := !h.IncidentOpen
	next.IncidentOpen = true
	return Transition{Next: next, OpenIncident: open}
}
