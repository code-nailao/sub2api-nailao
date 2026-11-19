-- Additive and disabled by default. Existing charges and group multipliers stay intact.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

ALTER TABLE groups ADD COLUMN IF NOT EXISTS upstream_rejection_billing_enabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE groups ADD COLUMN IF NOT EXISTS upstream_rejection_price NUMERIC(20,8) NOT NULL DEFAULT 0.1;

DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'groups_upstream_rejection_price_valid' AND conrelid = 'groups'::regclass) THEN
  ALTER TABLE groups ADD CONSTRAINT groups_upstream_rejection_price_valid CHECK (
   upstream_rejection_price >= 0 AND upstream_rejection_price < 1000000000000
   AND (NOT upstream_rejection_billing_enabled OR (upstream_rejection_price > 0 AND platform = 'grok' AND subscription_type = 'standard'))
  );
 END IF;
END $$;

COMMENT ON COLUMN groups.upstream_rejection_billing_enabled IS 'Explicit opt-in for verified upstream safety rejection billing; disabled for all existing groups';
COMMENT ON COLUMN groups.upstream_rejection_price IS 'Fixed final wallet amount, not multiplied by group, user, time or service-tier rates';

-- Preserve every existing invalidation condition and include the new monetary policy.
CREATE OR REPLACE FUNCTION enqueue_group_auth_cache_invalidation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    target_group_id BIGINT;
BEGIN
    target_group_id := OLD.id;
    IF TG_OP = 'UPDATE'
       AND OLD.status IS NOT DISTINCT FROM NEW.status
       AND OLD.is_exclusive IS NOT DISTINCT FROM NEW.is_exclusive
       AND OLD.allow_image_generation IS NOT DISTINCT FROM NEW.allow_image_generation
       AND OLD.platform IS NOT DISTINCT FROM NEW.platform
       AND OLD.subscription_type IS NOT DISTINCT FROM NEW.subscription_type
       AND OLD.rate_multiplier IS NOT DISTINCT FROM NEW.rate_multiplier
       AND OLD.peak_rate_enabled IS NOT DISTINCT FROM NEW.peak_rate_enabled
       AND OLD.peak_start IS NOT DISTINCT FROM NEW.peak_start
       AND OLD.peak_end IS NOT DISTINCT FROM NEW.peak_end
       AND OLD.peak_rate_multiplier IS NOT DISTINCT FROM NEW.peak_rate_multiplier
       AND OLD.profit_control_enabled IS NOT DISTINCT FROM NEW.profit_control_enabled
       AND OLD.profit_min_margin IS NOT DISTINCT FROM NEW.profit_min_margin
       AND OLD.profit_safety_buffer IS NOT DISTINCT FROM NEW.profit_safety_buffer
       AND OLD.upstream_rejection_billing_enabled IS NOT DISTINCT FROM NEW.upstream_rejection_billing_enabled
       AND OLD.upstream_rejection_price IS NOT DISTINCT FROM NEW.upstream_rejection_price
       AND OLD.deleted_at IS NOT DISTINCT FROM NEW.deleted_at THEN
        RETURN NEW;
    END IF;

    INSERT INTO auth_cache_invalidation_outbox (cache_key)
    SELECT encode(sha256(convert_to(k.key, 'UTF8')), 'hex')
    FROM api_keys AS k
    WHERE k.group_id = target_group_id
      AND k.deleted_at IS NULL
      AND k.key <> '';
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;
