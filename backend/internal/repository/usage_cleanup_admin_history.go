package repository

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// getAdminHomeModelHistory preserves only the unfiltered admin home contract.
// Summaries contain no request IDs or bodies and are never used for billing.
func (r *usageLogRepository) getAdminHomeModelHistory(ctx context.Context, start, end time.Time, source string) (result []ModelStat, err error) {
	const query = `SELECT model_name AS model, SUM(requests),
	 SUM(input_tokens), SUM(output_tokens), SUM(cache_creation_tokens), SUM(cache_read_tokens),
	 SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens),
	 SUM(total_cost), SUM(actual_cost), SUM(account_cost)
	 FROM usage_cleanup_model_source
	 WHERE created_at >= $1 AND created_at < $2 AND model_source = $3
	 GROUP BY model_name ORDER BY SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens) DESC`
	rows, err := r.sql.QueryContext(ctx, query, start, end, usagestats.NormalizeModelSource(source))
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			result, err = nil, closeErr
		}
	}()
	return scanModelStatsRows(rows)
}
