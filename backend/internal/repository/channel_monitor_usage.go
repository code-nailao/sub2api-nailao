package repository

import (
	"context"

	"github.com/lib/pq"
)

// ResolveUsageGroupsByAPIKeys uses current membership, including disabled keys
// that may still have usage this hour. Deleted keys/groups and unbound keys do
// not have a usable association. No credential leaves the service boundary.
func (r *channelMonitorRepository) ResolveUsageGroupsByAPIKeys(ctx context.Context, keys []string) (map[string]int64, error) {
	result := make(map[string]int64)
	if len(keys) == 0 {
		return result, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT k.key, k.group_id
FROM api_keys k JOIN groups g ON g.id = k.group_id
WHERE k.key = ANY($1) AND k.deleted_at IS NULL AND g.deleted_at IS NULL`, pq.Array(keys))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key string
		var group int64
		if err := rows.Scan(&key, &group); err != nil {
			return nil, err
		}
		result[key] = group
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
