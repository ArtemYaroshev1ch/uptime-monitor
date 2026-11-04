-- +goose Up
-- A new table only: the previous release does not know it and keeps working.
CREATE TABLE notification_channels (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type       TEXT NOT NULL CHECK (type IN ('telegram','webhook')),
    name       TEXT NOT NULL,
    -- telegram: {"chat_id": "..."}; webhook: {"url": "...", "secret": "..."}.
    -- The webhook secret signs deliveries; it is never returned by the API
    -- after the channel is created.
    config     JSONB NOT NULL,
    is_active  BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Every API query filters by owner, and fan-out looks up the active
-- channels of a monitor's owner.
CREATE INDEX idx_notification_channels_user ON notification_channels(user_id);

-- +goose Down
DROP TABLE notification_channels;
