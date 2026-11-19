package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV2DisplayOrderKeepsChosenOrderAndLegacyOmission(t *testing.T) {
	var legacy ChannelMonitorV2Config
	require.NoError(t, json.Unmarshal([]byte(`{"enabled":true,"refresh_interval_seconds":60}`), &legacy))
	require.NoError(t, normalizeChannelMonitorV2Config(&legacy))
	require.Nil(t, legacy.DisplayOrder)

	input := &ChannelMonitorV2DisplayOrder{
		Categories:      []string{"domestic", " OpenAI ", "domestic", "anthropic"},
		Groups:          map[string][]int64{"domestic": {224, 216, 224}, "openai": {155, 246, 8}},
		GroupCategories: map[string]string{"216": "domestic", "224": "DOMESTIC"},
	}
	order, err := NormalizeChannelMonitorV2DisplayOrder(input)
	require.NoError(t, err)
	require.Equal(t, []string{"domestic", "openai", "anthropic"}, order.Categories)
	require.Equal(t, []int64{224, 216}, order.Groups["domestic"])
	require.Equal(t, []int64{155, 246, 8}, order.Groups["openai"])
	require.Equal(t, "domestic", order.GroupCategories["224"])
	order.Groups["domestic"][0] = 1
	require.Equal(t, []int64{224, 216, 224}, input.Groups["domestic"], "normalization must not mutate shared config")

	reset, err := NormalizeChannelMonitorV2DisplayOrder(&ChannelMonitorV2DisplayOrder{})
	require.NoError(t, err)
	require.NotNil(t, reset, "empty object explicitly resets the stored order")
	require.Empty(t, reset.Categories)
	require.Empty(t, reset.Groups)
	require.Empty(t, reset.GroupCategories)
}

func TestChannelMonitorV2DisplayOrderRejectsMalformedLayout(t *testing.T) {
	tests := map[string]ChannelMonitorV2DisplayOrder{
		"unknown category":       {Categories: []string{"private-provider"}},
		"invalid group":          {Groups: map[string][]int64{"openai": {0}}},
		"ambiguous keys":         {Groups: map[string][]int64{"openai": {1}, "OpenAI": {2}}},
		"invalid override":       {GroupCategories: map[string]string{"216": "invalid"}},
		"negative ID":            {GroupCategories: map[string]string{"-1": "domestic"}},
		"ambiguous ID":           {GroupCategories: map[string]string{"0216": "domestic"}},
		"too many entries":       {Groups: map[string][]int64{"openai": make([]int64, channelMonitorV2MaxDisplayGroups+1)}},
		"eighth category":        {Groups: map[string][]int64{"openai": {}, "anthropic": {}, "grok": {}, "gemini": {}, "domestic": {}, "image": {}, "other": {}, "extra": {}}},
		"invalid image grouping": {ImageGrouping: "combined"},
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NormalizeChannelMonitorV2DisplayOrder(&input)
			require.ErrorIs(t, err, ErrChannelMonitorV2InvalidConfig)
		})
	}
}

func TestChannelMonitorV2DisplayOrderImageGrouping(t *testing.T) {
	for _, mode := range []string{"", "separate", "provider"} {
		name := mode
		if name == "" {
			name = "legacy default remains omitted"
		}
		t.Run(name, func(t *testing.T) {
			input := &ChannelMonitorV2DisplayOrder{
				Categories: []string{"openai", "anthropic", "grok", "gemini", "domestic", "image", "other"},
				Groups: map[string][]int64{
					"openai": {246}, "anthropic": {48}, "grok": {132}, "gemini": {59},
					"domestic": {216}, "image": {121, 74}, "other": {},
				},
				GroupCategories: map[string]string{"121": "openai", "74": "image"},
				ImageGrouping:   mode,
			}
			order, err := NormalizeChannelMonitorV2DisplayOrder(input)
			require.NoError(t, err)
			require.Equal(t, input, order, "normalization must preserve explicit overrides regardless of grouping preference")
			public := channelMonitorV2DisplayOrderForViewer(order, ChannelMonitorV2Filter{RestrictGroups: true, AllowedGroupIDs: []int64{121}}, nil)
			require.Equal(t, mode, public.ImageGrouping)
			require.Equal(t, map[string]string{"121": "openai"}, public.GroupCategories)
			require.Equal(t, []int64{121}, public.Groups["image"])
			encoded, err := json.Marshal(public)
			require.NoError(t, err)
			if mode == "" {
				require.NotContains(t, string(encoded), "image_grouping")
			} else {
				require.Contains(t, string(encoded), `"image_grouping":"`+mode+`"`)
			}
		})
	}
}

func TestChannelMonitorV2DisplayOrderSupportsOtherCategory(t *testing.T) {
	input := &ChannelMonitorV2DisplayOrder{
		Categories: []string{"other", "domestic", "openai", "anthropic", "grok", "gemini"},
		Groups: map[string][]int64{
			"other": {121, 74}, "domestic": {216}, "openai": {246},
			"anthropic": {48}, "grok": {132}, "gemini": {59},
		},
		GroupCategories: map[string]string{"121": "other", "74": "other"},
	}
	order, err := NormalizeChannelMonitorV2DisplayOrder(input)
	require.NoError(t, err)
	require.Equal(t, input, order)
	public := channelMonitorV2DisplayOrderForViewer(order, ChannelMonitorV2Filter{RestrictGroups: true, AllowedGroupIDs: []int64{121}}, nil)
	require.Equal(t, []int64{121}, public.Groups["other"])
	require.Equal(t, map[string]string{"121": "other"}, public.GroupCategories)
}

func TestChannelMonitorV2SnapshotScopesDisplayOrderWithoutMutatingAdminConfig(t *testing.T) {
	order := &ChannelMonitorV2DisplayOrder{
		Categories:      []string{"domestic", "openai"},
		Groups:          map[string][]int64{"domestic": {216, 224}, "openai": {155, 246, 8}},
		GroupCategories: map[string]string{"216": "domestic", "224": "domestic"},
	}
	repo := &channelMonitorV2RepoStub{
		config: ChannelMonitorV2Config{Enabled: true, GroupIDs: []int64{155, 216, 246}},
		snap:   &ChannelMonitorV2Snapshot{Config: ChannelMonitorV2Config{DisplayOrder: order}},
	}
	svc := NewChannelMonitorV2Service(repo)
	filter := ChannelMonitorV2Filter{RestrictGroups: true, AllowedGroupIDs: []int64{216, 246, 224}, GroupIDs: []int64{216}}
	public, err := svc.Snapshot(context.Background(), filter, false)
	require.NoError(t, err)
	require.Equal(t, []string{"domestic", "openai"}, public.Config.DisplayOrder.Categories)
	require.Equal(t, map[string][]int64{"domestic": {216}, "openai": {246}}, public.Config.DisplayOrder.Groups)
	require.Equal(t, map[string]string{"216": "domestic"}, public.Config.DisplayOrder.GroupCategories)
	require.Equal(t, []int64{216, 224}, order.Groups["domestic"])
	admin, err := svc.Snapshot(context.Background(), ChannelMonitorV2Filter{}, true)
	require.NoError(t, err)
	require.Equal(t, order, admin.Config.DisplayOrder)

	for _, filter := range []ChannelMonitorV2Filter{{}, {RestrictGroups: true}} {
		public, err := svc.Snapshot(context.Background(), filter, false)
		require.NoError(t, err)
		require.Empty(t, public.Config.DisplayOrder.Groups)
		require.Empty(t, public.Config.DisplayOrder.GroupCategories)
	}
}

func TestChannelMonitorV2MetricPresenceSurvivesRedaction(t *testing.T) {
	tests := []struct {
		name     string
		metric   ChannelMonitorV2Metric
		requests bool
		cache    bool
	}{
		{name: "no traffic"},
		{name: "requests without cache input", metric: ChannelMonitorV2Metric{RequestCount: 5}, requests: true},
		{name: "valid zero cache hit", metric: ChannelMonitorV2Metric{RequestCount: 5, CacheRateDenominator: 20}, requests: true, cache: true},
		{name: "cache hit", metric: ChannelMonitorV2Metric{RequestCount: 5, CacheRateDenominator: 20, CacheRateNumerator: 10, CacheRate: 0.5}, requests: true, cache: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metric := test.metric
			for i := 0; i < 2; i++ {
				redactChannelMonitorV2Metric(&metric, true)
				require.Equal(t, test.requests, metric.HasRequests)
				require.Equal(t, test.cache, metric.HasCacheInput)
				require.Zero(t, metric.RequestCount)
				require.Zero(t, metric.CacheRateDenominator)
				require.Zero(t, metric.CacheRateNumerator)
				require.Equal(t, test.metric.CacheRate, metric.CacheRate)
			}
		})
	}
}
