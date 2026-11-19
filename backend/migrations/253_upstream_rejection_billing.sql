-- Opt-in durable holds and append-only billing evidence. Existing balances,
-- historical frozen funds, usage records and charging policies are unchanged.
CREATE TABLE IF NOT EXISTS upstream_rejection_reservations (
    request_id VARCHAR(80) PRIMARY KEY,
    user_id BIGINT NOT NULL,
    api_key_id BIGINT NOT NULL,
    account_id BIGINT NOT NULL,
    group_id BIGINT NOT NULL,
    price NUMERIC(20,8) NOT NULL CHECK (price > 0),
    fingerprint VARCHAR(64) NOT NULL,
    snapshot JSONB NOT NULL,
    state VARCHAR(20) NOT NULL CHECK (state IN ('reserved','sent','unknown','rejected','released','normal')),
    finalization_fingerprint VARCHAR(64) NOT NULL DEFAULT '',
    evidence JSONB NOT NULL DEFAULT '{}',
    task_id VARCHAR(256),
    task_metadata JSONB,
    normal_billing_request_id TEXT NOT NULL DEFAULT '',
    sent_at TIMESTAMPTZ,
    usage_projected_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_upstream_rejection_pending_key
    ON upstream_rejection_reservations(api_key_id, created_at)
    WHERE state IN ('reserved','sent','unknown');
CREATE INDEX IF NOT EXISTS idx_upstream_rejection_pending
    ON upstream_rejection_reservations(updated_at, request_id)
    WHERE state IN ('reserved','sent','unknown');
CREATE INDEX IF NOT EXISTS idx_upstream_rejection_unprojected
    ON upstream_rejection_reservations(updated_at, request_id)
    WHERE state='rejected' AND usage_projected_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_upstream_rejection_user_pending
    ON upstream_rejection_reservations(user_id, created_at DESC, request_id DESC)
    WHERE state IN ('reserved','sent','unknown') AND price>0;
CREATE UNIQUE INDEX IF NOT EXISTS idx_upstream_rejection_task
    ON upstream_rejection_reservations(account_id, task_id)
    WHERE task_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_upstream_rejection_owned_task
    ON upstream_rejection_reservations(user_id, api_key_id, task_id)
    WHERE task_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS upstream_rejection_billing_events (
    id BIGSERIAL PRIMARY KEY,
    request_id VARCHAR(80) NOT NULL REFERENCES upstream_rejection_reservations(request_id),
    kind VARCHAR(20) NOT NULL CHECK (kind IN ('reserved','sent','unknown','rejected','released','normal','task_bound')),
    api_key_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL,
    amount NUMERIC(20,8) NOT NULL CHECK (amount >= 0),
    snapshot JSONB NOT NULL,
    evidence JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(request_id, kind)
);
COMMENT ON TABLE upstream_rejection_billing_events IS 'Append-only fixed rejection fee evidence; created in the same transaction as the corresponding balance and dedup mutation.';
