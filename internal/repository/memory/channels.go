package memory

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
)

// Channels is an in-memory notification channel repository.
type Channels struct {
	failable
	channels []domain.Channel
	now      func() time.Time
}

// NewChannels returns an empty Channels. now stamps CreatedAt.
func NewChannels(now func() time.Time) *Channels {
	return &Channels{now: now}
}

// Create stores a channel unless the user has limit channels already.
func (r *Channels) Create(_ context.Context, userID uuid.UUID, ch domain.Channel, limit int) (domain.Channel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return domain.Channel{}, r.err
	}
	count := 0
	for _, c := range r.channels {
		if c.UserID == userID {
			count++
		}
	}
	if count >= limit {
		return domain.Channel{}, domain.ErrLimitExceeded
	}
	ch.ID, ch.UserID, ch.CreatedAt = uuid.New(), userID, r.now()
	r.channels = append(r.channels, ch)
	return ch, nil
}

// Get returns the user's channel or domain.ErrNotFound.
func (r *Channels) Get(_ context.Context, userID, id uuid.UUID) (domain.Channel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return domain.Channel{}, r.err
	}
	i := r.index(userID, id)
	if i < 0 {
		return domain.Channel{}, domain.ErrNotFound
	}
	return r.channels[i], nil
}

// List returns a page of the user's channels, newest first, and their total.
func (r *Channels) List(_ context.Context, userID uuid.UUID, limit, offset int) ([]domain.Channel, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, 0, r.err
	}
	var own []domain.Channel
	for i := len(r.channels) - 1; i >= 0; i-- {
		if r.channels[i].UserID == userID {
			own = append(own, r.channels[i])
		}
	}
	total := len(own)
	return own[min(offset, total):min(offset+limit, total)], total, nil
}

// Update applies fn to the user's channel and stores the result unless fn
// fails.
func (r *Channels) Update(_ context.Context, userID, id uuid.UUID, fn func(*domain.Channel) error) (domain.Channel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return domain.Channel{}, r.err
	}
	i := r.index(userID, id)
	if i < 0 {
		return domain.Channel{}, domain.ErrNotFound
	}
	ch := r.channels[i]
	if err := fn(&ch); err != nil {
		return domain.Channel{}, err
	}
	r.channels[i] = ch
	return ch, nil
}

// Delete removes the user's channel or returns domain.ErrNotFound.
func (r *Channels) Delete(_ context.Context, userID, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	i := r.index(userID, id)
	if i < 0 {
		return domain.ErrNotFound
	}
	r.channels = slices.Delete(r.channels, i, i+1)
	return nil
}

func (r *Channels) index(userID, id uuid.UUID) int {
	return slices.IndexFunc(r.channels, func(c domain.Channel) bool { return c.ID == id && c.UserID == userID })
}
