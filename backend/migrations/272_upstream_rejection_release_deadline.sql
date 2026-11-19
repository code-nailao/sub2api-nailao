-- Durable release deadline for synchronous rejection-fee holds whose outcome
-- stayed unknown. Additive only. A sent or unknown row that was already
-- waiting when this migration ran keeps a NULL deadline forever: those holds
-- are reconciled by hand and are never released by the timer. Task-bound rows
-- keep a NULL deadline because the task settles them. The partial index for
-- due rows is built without blocking writers in the following _notx migration.
SET LOCAL lock_timeout = '5s';

ALTER TABLE upstream_rejection_reservations
    ADD COLUMN IF NOT EXISTS release_after TIMESTAMPTZ,
    -- A result that arrived after the timer released the hold, kept once for
    -- supplier reconciliation.
    ADD COLUMN IF NOT EXISTS late_result JSONB;
