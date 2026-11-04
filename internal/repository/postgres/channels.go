package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
)

// Channels stores notification channels. Like Monitors, every method is
// scoped to the owner: another user's channel is domain.ErrNotFound.
type Channels struct {
	db TxDB
}

// NewChannels returns a Channels repository.
func NewChannels(db TxDB) *Channels {
	return &Channels{db: db}
}

const channelColumns = `c.id, c.user_id, c.type, c.name, c.config, c.is_active, c.created_at`

// scanChannel reads channelColumns. pgx decodes the JSONB config into the
// struct with encoding/json.
func scanChannel(row pgx.Row) (domain.Channel, error) {
	var ch domain.Channel
	err := row.Scan(&ch.ID, &ch.UserID, &ch.Type, &ch.Name, &ch.Config, &ch.IsActive, &ch.CreatedAt)
	return ch, err
}

// Create inserts a channel unless the user already has limit channels
// (domain.ErrLimitExceeded). The owner's row is locked for the count, as in
// Monitors.Create.
func (r *Channels) Create(ctx context.Context, userID uuid.UUID, ch domain.Channel, limit int) (domain.Channel, error) {
	var out domain.Channel
	err := pgx.BeginFunc(ctx, r.db, func(tx pgx.Tx) error {
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&locked); err != nil {
			return notFound("lock user", err)
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM notification_channels WHERE user_id = $1`, userID).Scan(&count); err != nil {
			return fmt.Errorf("count channels: %w", err)
		}
		if count >= limit {
			return fmt.Errorf("%d channels: %w", count, domain.ErrLimitExceeded)
		}

		var err error
		out, err = scanChannel(tx.QueryRow(ctx, `
			INSERT INTO notification_channels AS c (user_id, type, name, config, is_active)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING `+channelColumns,
			userID, ch.Type, ch.Name, ch.Config, ch.IsActive))
		if err != nil {
			return fmt.Errorf("insert channel: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.Channel{}, fmt.Errorf("create channel: %w", err)
	}
	return out, nil
}

// Get returns one channel of the user or domain.ErrNotFound.
func (r *Channels) Get(ctx context.Context, userID, id uuid.UUID) (domain.Channel, error) {
	ch, err := scanChannel(r.db.QueryRow(ctx,
		`SELECT `+channelColumns+` FROM notification_channels c WHERE c.id = $1 AND c.user_id = $2`, id, userID))
	if err != nil {
		return domain.Channel{}, notFound("get channel", err)
	}
	return ch, nil
}

// List returns a page of the user's channels, newest first, and their total.
func (r *Channels) List(ctx context.Context, userID uuid.UUID, limit, offset int) ([]domain.Channel, int, error) {
	var total int
	if err := r.db.QueryRow(ctx, `SELECT count(*) FROM notification_channels WHERE user_id = $1`, userID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count channels: %w", err)
	}
	rows, err := r.db.Query(ctx, `
		SELECT `+channelColumns+` FROM notification_channels c
		WHERE c.user_id = $1
		ORDER BY c.created_at DESC, c.id DESC
		LIMIT $2 OFFSET $3`,
		userID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list channels: %w", err)
	}
	channels, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Channel, error) {
		return scanChannel(row)
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list channels: %w", err)
	}
	return channels, total, nil
}

// Update loads the user's channel under a row lock, lets fn change its name,
// config and is_active, and saves it. An error from fn aborts the update.
func (r *Channels) Update(ctx context.Context, userID, id uuid.UUID, fn func(*domain.Channel) error) (domain.Channel, error) {
	var ch domain.Channel
	err := pgx.BeginFunc(ctx, r.db, func(tx pgx.Tx) error {
		var err error
		ch, err = scanChannel(tx.QueryRow(ctx,
			`SELECT `+channelColumns+` FROM notification_channels c WHERE c.id = $1 AND c.user_id = $2 FOR UPDATE`,
			id, userID))
		if err != nil {
			return notFound("get channel", err)
		}
		if err := fn(&ch); err != nil {
			return err
		}
		ch, err = scanChannel(tx.QueryRow(ctx, `
			UPDATE notification_channels AS c SET name = $3, config = $4, is_active = $5
			WHERE c.id = $1 AND c.user_id = $2
			RETURNING `+channelColumns,
			id, userID, ch.Name, ch.Config, ch.IsActive))
		if err != nil {
			return fmt.Errorf("save channel: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.Channel{}, fmt.Errorf("update channel: %w", err)
	}
	return ch, nil
}

// Delete removes the user's channel and, by cascade, its notifications.
func (r *Channels) Delete(ctx context.Context, userID, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM notification_channels WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("delete channel: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete channel: %w", domain.ErrNotFound)
	}
	return nil
}
