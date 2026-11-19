-- Upstream display wallets and administrator alerts only. No model credentials,
-- user balances, charging records or existing wallet snapshots are changed.
CREATE TABLE IF NOT EXISTS sources_wallet_management (
    id SMALLINT PRIMARY KEY CHECK (id = 1),
    value JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
INSERT INTO sources_wallet_management(id, value)
VALUES (1, '{"version":1,"profiles":{},"members":{},"sources":{},"deliveries":{}}'::jsonb)
ON CONFLICT (id) DO NOTHING;
