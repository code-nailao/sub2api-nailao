package repository

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

func requestSpeedGroupPointers(groups []service.Group) []*service.Group {
	out := make([]*service.Group, len(groups))
	for i := range groups {
		out[i] = &groups[i]
	}
	return out
}

// Availability is presentation metadata, never a replacement for scheduler
// health/transport/model checks. Keep it out of authentication snapshots.
func (r *groupRepository) hydrateRequestSpeedAvailability(ctx context.Context, groups []*service.Group) error {
	byID := make(map[int64]*service.Group, len(groups))
	ids := make([]int64, 0, len(groups))
	for _, g := range groups {
		g.AvailableRequestSpeedTiers = []string{}
		if g.RequestSpeedSelectionEnabled && g.Platform == service.PlatformOpenAI {
			byID[g.ID] = g
			ids = append(ids, g.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := r.sql.QueryContext(ctx, `SELECT DISTINCT ag.group_id, tier.value
		FROM account_groups ag JOIN accounts a ON a.id = ag.account_id
		CROSS JOIN LATERAL jsonb_array_elements_text(ag.request_speed_tiers) AS tier(value)
		WHERE ag.group_id = ANY($1) AND a.deleted_at IS NULL
		AND a.platform = 'openai' AND a.type = 'apikey' AND a.status = 'active'
		AND a.schedulable = TRUE
		AND tier.value IN ('fast', 'ultrafast')
		ORDER BY ag.group_id, tier.value`, pq.Array(ids))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var tier string
		if err := rows.Scan(&id, &tier); err != nil {
			return err
		}
		if g := byID[id]; g != nil {
			g.AvailableRequestSpeedTiers = append(g.AvailableRequestSpeedTiers, tier)
		}
	}
	return rows.Err()
}
