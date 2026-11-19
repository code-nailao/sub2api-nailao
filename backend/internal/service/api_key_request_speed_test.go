//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIKeyRequestSpeedUpdateIsExplicitAndScoped(t *testing.T) {
	groupID := int64(2)
	fast := "fast"
	for _, tt := range []struct {
		name   string
		req    UpdateAPIKeyRequest
		writes bool
		reset  bool
	}{
		{name: "omit preserves"},
		{name: "explicit fast", req: UpdateAPIKeyRequest{RequestSpeedTier: &fast}, writes: true},
		{name: "null reset", req: UpdateAPIKeyRequest{RequestSpeedTierSet: true}, writes: true, reset: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, repo := newUpdateFieldsAPIKeyService(&APIKey{ID: 1, UserID: 7, GroupID: &groupID, Key: "sk-test", Status: StatusActive})
			_, err := svc.Update(context.Background(), 1, 7, tt.req)
			require.NoError(t, err)
			require.Len(t, repo.updateFields, 1)
			fields := repo.updateFields[0]
			require.Equal(t, tt.writes, fields.RequestSpeedTier)
			require.Equal(t, tt.writes, fields.CheckExpectedGroupID)
			require.Equal(t, tt.reset, fields.ResolveRequestSpeedDefault)
			if tt.writes {
				require.Equal(t, &groupID, fields.ExpectedGroupID)
			}
			require.False(t, fields.QuotaUsed)
			require.False(t, fields.RateLimitUsage)
		})
	}
	invalid := "priority"
	require.ErrorIs(t, validateCreateAPIKeyRequest(CreateAPIKeyRequest{RequestSpeedTier: &invalid}), ErrRequestSpeedInvalid)
	require.ErrorIs(t, validateUpdateAPIKeyRequest(UpdateAPIKeyRequest{RequestSpeedTier: &invalid}), ErrRequestSpeedInvalid)
}
