package handler

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIKeyRequestSpeedOmittedNullAndExplicit(t *testing.T) {
	for _, tt := range []struct {
		body string
		set  bool
		tier *string
	}{
		{body: `{"name":"unchanged"}`},
		{body: `{"request_speed_tier":null}`, set: true},
		{body: `{"request_speed_tier":"fast"}`, set: true, tier: stringPointerForSpeedTest("fast")},
	} {
		var request UpdateAPIKeyRequest
		require.NoError(t, json.Unmarshal([]byte(tt.body), &request))
		require.Equal(t, tt.set, request.RequestSpeedTierSet)
		require.Equal(t, tt.tier, request.RequestSpeedTier)
	}
	var request UpdateAPIKeyRequest
	require.Error(t, json.Unmarshal([]byte(`{"request_speed_tier":true}`), &request))
}

func stringPointerForSpeedTest(v string) *string { return &v }
