package repository

import (
	"encoding/json"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const channelMonitorV2DisplayOrderSettingKey = "sources_channel_monitor_v2_display_order"

func decodeChannelMonitorV2DisplayOrder(raw string) *service.ChannelMonitorV2DisplayOrder {
	var order *service.ChannelMonitorV2DisplayOrder
	if err := json.Unmarshal([]byte(raw), &order); err != nil {
		// Display-only corruption must not disable the monitor's aggregation
		// worker or existing metrics. The UI falls back to its default order.
		return nil
	}
	normalized, err := service.NormalizeChannelMonitorV2DisplayOrder(order)
	if err != nil {
		return nil
	}
	return normalized
}
