-- Opt-in only: existing groups and keys retain their prior behavior.
ALTER TABLE groups ADD COLUMN IF NOT EXISTS deepseek_tools JSONB NOT NULL DEFAULT '{}';
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS deepseek_tool_mode VARCHAR(16) NOT NULL DEFAULT 'inherit';
ALTER TABLE api_keys ADD CONSTRAINT api_keys_deepseek_tool_mode_check CHECK (deepseek_tool_mode IN ('inherit', 'enabled', 'disabled'));
-- Request-local confirmed usage checkpoints. Recovery only settles stored
-- evidence; it never repeats an upstream operation or reconstructs credentials.
CREATE TABLE IF NOT EXISTS deepseek_usage_journal (
    request_id TEXT NOT NULL,
    api_key_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL,
    account_id BIGINT NOT NULL,
    deadline TIMESTAMPTZ NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','ready','settled')),
    revision INTEGER NOT NULL DEFAULT 0,
    command JSONB,
    usage_log JSONB,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (request_id, api_key_id)
);
CREATE INDEX IF NOT EXISTS idx_deepseek_usage_pending ON deepseek_usage_journal (updated_at, request_id) WHERE state <> 'settled';
