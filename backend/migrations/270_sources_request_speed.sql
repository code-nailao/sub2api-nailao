-- Additive native request-speed configuration. Existing keys remain nullable;
-- enabled groups interpret those legacy keys as Ultrafast without rewriting them.
ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS request_speed_selection_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS default_request_speed_tier VARCHAR(16) NOT NULL DEFAULT 'ultrafast';
ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS request_speed_tier VARCHAR(16);
ALTER TABLE account_groups
    ADD COLUMN IF NOT EXISTS request_speed_tiers JSONB NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE groups ADD CONSTRAINT groups_request_speed_default_check
    CHECK (default_request_speed_tier IN ('fast', 'ultrafast'));
ALTER TABLE groups ADD CONSTRAINT groups_request_speed_platform_check
    CHECK (NOT request_speed_selection_enabled OR platform = 'openai');
ALTER TABLE api_keys ADD CONSTRAINT api_keys_request_speed_tier_check
    CHECK (request_speed_tier IS NULL OR request_speed_tier IN ('fast', 'ultrafast'));
ALTER TABLE account_groups ADD CONSTRAINT account_groups_request_speed_tiers_check
    CHECK (jsonb_typeof(request_speed_tiers) = 'array'
        AND request_speed_tiers <@ '["fast", "ultrafast"]'::jsonb);
