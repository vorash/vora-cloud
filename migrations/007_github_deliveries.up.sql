-- Webhook deliveries received from GitHub, by the id GitHub gives each one
-- (X-GitHub-Delivery). A delivery id seen twice is a replay or a redelivery
-- and is not processed again. Rows are only needed for as long as GitHub can
-- redeliver, so the service deletes old ones.
CREATE TABLE github_deliveries (
    delivery_id TEXT PRIMARY KEY,
    event TEXT NOT NULL,
    received_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_github_deliveries_received_at ON github_deliveries(received_at);

ALTER TABLE github_deliveries ENABLE ROW LEVEL SECURITY;
