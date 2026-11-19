package repository

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.UserPendingUpstreamRejectionReader = (*usageBillingRepository)(nil)

// The cursor is a position, never authorization. Its owner must match the
// authenticated user and the SQL always repeats that user_id predicate. A user
// can select another position in their own history without acquiring authority
// over any other user's rows; no application signing secret is needed here.
type rejectionUserCursor struct {
	Version   int       `json:"v"`
	UserID    int64     `json:"u"`
	CreatedAt time.Time `json:"t"`
	RequestID string    `json:"r"`
}

func (r *usageBillingRepository) ListUserPendingUpstreamRejections(ctx context.Context, userID int64, cursor string, pageSize int) (*service.UserPendingUpstreamRejectionsPage, error) {
	if userID <= 0 || pageSize < 1 || pageSize > 50 || len(cursor) > 1024 {
		return nil, service.ErrUpstreamRejectionInvalid
	}
	if err := r.rejectionDB(); err != nil {
		return nil, err
	}
	where := ` WHERE user_id=$1 AND price>0 AND state IN ('reserved','sent','unknown')`
	args := []any{userID, pageSize + 1}
	if cursor != "" {
		payload, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return nil, service.ErrUpstreamRejectionInvalid
		}
		var after rejectionUserCursor
		if json.Unmarshal(payload, &after) != nil || after.Version != 1 || after.UserID != userID || after.CreatedAt.IsZero() || !service.IsUpstreamRejectionRequestID(after.RequestID) {
			return nil, service.ErrUpstreamRejectionInvalid
		}
		where += ` AND (created_at,request_id)<($3,$4)`
		args = append(args, after.CreatedAt, after.RequestID)
	}
	// Select only fields intended for the customer's UI, not the private ledger
	// snapshot containing the supplier identity digest and acquisition cost.
	rows, err := r.db.QueryContext(ctx, `SELECT request_id,snapshot->>'model',state,price,created_at FROM upstream_rejection_reservations`+where+` ORDER BY created_at DESC,request_id DESC LIMIT $2`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	page := &service.UserPendingUpstreamRejectionsPage{Items: make([]service.UserPendingUpstreamRejection, 0, pageSize)}
	for rows.Next() {
		var row service.UserPendingUpstreamRejection
		if err := rows.Scan(&row.RequestID, &row.Model, &row.State, &row.HeldPrice, &row.CreatedAt); err != nil {
			return nil, err
		}
		page.Items = append(page.Items, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Items) > pageSize {
		page.Items = page.Items[:pageSize]
		last := page.Items[len(page.Items)-1]
		encoded, err := json.Marshal(rejectionUserCursor{Version: 1, UserID: userID, CreatedAt: last.CreatedAt, RequestID: last.RequestID})
		if err != nil {
			return nil, err
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return page, nil
}
