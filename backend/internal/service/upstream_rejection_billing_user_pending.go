package service

import (
	"context"
	"time"
)

// This public projection deliberately omits supplier cost, credentials, account
// identity and raw evidence. The authenticated user ID is supplied separately.
type UserPendingUpstreamRejection struct {
	RequestID string    `json:"request_id"`
	Model     string    `json:"model"`
	State     string    `json:"state"`
	HeldPrice float64   `json:"held_price"`
	CreatedAt time.Time `json:"created_at"`
}

type UserPendingUpstreamRejectionsPage struct {
	Items      []UserPendingUpstreamRejection `json:"items"`
	NextCursor string                         `json:"next_cursor,omitempty"`
}

// Read-only optional interface; customer handlers never receive settlement
// methods and do not need to enumerate all users' pending work then filter it.
type UserPendingUpstreamRejectionReader interface {
	ListUserPendingUpstreamRejections(context.Context, int64, string, int) (*UserPendingUpstreamRejectionsPage, error)
}
