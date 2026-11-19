-- Display-only upstream wallet snapshots. No API keys, billing or account state.
CREATE TABLE IF NOT EXISTS sources_account_wallet_snapshots (
    account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    identity VARCHAR(64) NOT NULL,
    value JSONB NOT NULL DEFAULT '{"status":"pending","stale":false}'::jsonb,
    next_check_at TIMESTAMPTZ,
    last_started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_token VARCHAR(36),
    lease_until TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_sources_wallet_next_check
    ON sources_account_wallet_snapshots(next_check_at)
    WHERE next_check_at IS NOT NULL;
