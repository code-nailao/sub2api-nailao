-- Preserve ordering already saved by the earlier private release. Only a fresh
-- upstream schema needs a backfill, using its previous admin order (id DESC).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'channel_monitors' AND column_name = 'sort_order'
    ) THEN
        ALTER TABLE channel_monitors ADD COLUMN sort_order INT NOT NULL DEFAULT 0;
        WITH ordered AS (
            SELECT id, (ROW_NUMBER() OVER (ORDER BY id DESC) - 1) * 10 AS position
            FROM channel_monitors
        )
        UPDATE channel_monitors AS monitor SET sort_order = ordered.position
        FROM ordered WHERE monitor.id = ordered.id;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_channel_monitors_sort_order
    ON channel_monitors (sort_order);
