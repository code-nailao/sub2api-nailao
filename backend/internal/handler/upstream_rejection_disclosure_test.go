package handler

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUpstreamRejectionDisclosureDTOsDoNotApplyMultipliers(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		available := filterUserVisibleGroups([]service.AvailableGroupRef{{ID: 1, RateMultiplier: 0.2,
			UpstreamRejectionBillingEnabled: enabled, UpstreamRejectionPrice: 0.1}}, map[int64]struct{}{1: {}})
		require.Len(t, available, 1)
		plaza := toModelPlazaGroupDTO(&service.PlazaGroup{ID: 1, RateMultiplier: 0.2,
			UpstreamRejectionBillingEnabled: enabled, UpstreamRejectionPrice: 0.1}, map[int64]float64{1: 0.05})
		for _, view := range []any{available[0], plaza} {
			data, err := json.Marshal(view)
			require.NoError(t, err)
			var fields map[string]any
			require.NoError(t, json.Unmarshal(data, &fields))
			require.Equal(t, enabled, fields["upstream_rejection_billing_enabled"])
			require.Equal(t, 0.1, fields["upstream_rejection_price"])
		}
	}
}
