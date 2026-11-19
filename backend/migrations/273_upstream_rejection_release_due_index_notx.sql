-- Due synchronous holds for the deadline sweep. Built CONCURRENTLY so requests
-- that write holds are never blocked while it builds.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_upstream_rejection_release_due
    ON upstream_rejection_reservations (release_after, request_id)
    WHERE state IN ('sent','unknown') AND task_id IS NULL AND release_after IS NOT NULL;
