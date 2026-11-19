package dto

import (
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestDeepSeekCustomerReceiptHidesSupplier(t *testing.T) {
	snapshot := &service.UsageBillingSnapshot{Version: 1, ChargeKind: "deepseek_tools", DeepSeekTools: &service.DeepSeekToolUsage{SearchCount: 1, FetchCount: 2, SearchPrice: .006, FetchPrice: .006, SupplierAccountID: 7181, SupplierSearchPrice: .005, SupplierFetchPrice: .005}}
	wire, err := json.Marshal(usageBillingSnapshotForUser(snapshot))
	require.NoError(t, err)
	require.NotContains(t, string(wire), "supplier")
	require.NotContains(t, string(wire), "7181")
	require.NotContains(t, string(wire), "0.005")
	require.Contains(t, string(wire), `"fetch_count":2`)
	require.Contains(t, string(wire), `"search_price_per_call":0.006`)
}
