-- +goose Up
-- The notification outbox. A new table only: the previous release does not
-- know it and keeps working (it simply sends nothing).
--
-- Rows are inserted in the same transaction as the change they report (an
-- incident opened or resolved, a certificate about to expire), so a
-- notification is never lost and never sent for a change that rolled back.
-- The notifier loop delivers them afterwards.
CREATE TABLE notifications (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    monitor_id      UUID NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    -- NULL for cert_expiring.
    incident_id     UUID REFERENCES incidents(id) ON DELETE CASCADE,
    channel_id      UUID NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    event           TEXT NOT NULL CHECK (event IN ('incident_opened','incident_resolved','cert_expiring')),
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','sent','failed')),
    -- Delivery attempts made so far.
    attempts        INT  NOT NULL DEFAULT 0,
    -- When the notifier may take the row: the retry time after a failure,
    -- or the end of the lease while a worker is sending it.
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at         TIMESTAMPTZ
);

-- The notifier looks for pending rows that are due. Partial: sent and
-- failed rows, the vast majority, are not in the index at all.
CREATE INDEX idx_notifications_pending ON notifications(next_attempt_at) WHERE status = 'pending';

-- +goose Down
DROP TABLE notifications;
