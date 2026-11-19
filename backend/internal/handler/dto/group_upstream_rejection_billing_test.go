package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGroupDTOAlwaysDisclosesUpstreamRejectionPrice(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		group := &service.Group{ID: 1, UpstreamRejectionBillingEnabled: enabled, UpstreamRejectionPrice: 0.1, RateMultiplier: 0.2}
		for _, dto := range []any{GroupFromService(group), GroupFromServiceShallow(group), GroupFromServiceAdmin(group)} {
			data, err := json.Marshal(dto)
			require.NoError(t, err)
			var fields map[string]any
			require.NoError(t, json.Unmarshal(data, &fields))
			require.Equal(t, enabled, fields["upstream_rejection_billing_enabled"])
			require.Equal(t, 0.1, fields["upstream_rejection_price"])
			require.Equal(t, 0.2, fields["rate_multiplier"])
		}
	}
}
