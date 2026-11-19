-- Additive only: old binaries ignore these fields; never rewrite historical charges.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
ALTER TABLE channel_model_pricing ADD COLUMN IF NOT EXISTS ultrafast_multiplier NUMERIC(12,6);
ALTER TABLE groups ADD COLUMN IF NOT EXISTS openai_service_tier_policy VARCHAR(24);
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS billing_snapshot JSONB;
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='channel_model_pricing_ultrafast_multiplier_positive' AND conrelid='channel_model_pricing'::regclass) THEN
  ALTER TABLE channel_model_pricing ADD CONSTRAINT channel_model_pricing_ultrafast_multiplier_positive CHECK (ultrafast_multiplier IS NULL OR ultrafast_multiplier > 0);
 END IF;
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='groups_openai_service_tier_policy_valid' AND conrelid='groups'::regclass) THEN
  ALTER TABLE groups ADD CONSTRAINT groups_openai_service_tier_policy_valid CHECK (openai_service_tier_policy IS NULL OR openai_service_tier_policy IN ('','passthrough','force_fast','force_ultrafast'));
 END IF;
END $$;
COMMENT ON COLUMN groups.openai_service_tier_policy IS 'Request-tier selection only; independent of group and price multipliers';
COMMENT ON COLUMN usage_logs.billing_snapshot IS 'Immutable per-request tier and effective-price evidence; NULL for legacy records';
