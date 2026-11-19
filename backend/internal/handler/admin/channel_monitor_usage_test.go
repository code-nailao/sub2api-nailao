//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type monitorUsageHandlerRepo struct {
	service.ChannelMonitorRepository
	group int64
	fail  bool
	calls int
	keys  []string
}

func (r *monitorUsageHandlerRepo) List(context.Context, service.ChannelMonitorListParams) ([]*service.ChannelMonitor, int64, error) {
	return []*service.ChannelMonitor{
		{ID: 104, Name: "new monitor", APIKey: "ENC:private-monitor-key", PrimaryModel: "m", Enabled: true},
		{ID: 105, Name: "same key", APIKey: "ENC:private-monitor-key", PrimaryModel: "m", Enabled: true},
		{ID: 106, Name: "external", APIKey: "ENC:external-key", PrimaryModel: "m", Enabled: true},
	}, 3, nil
}
func (*monitorUsageHandlerRepo) ListLatestForMonitorIDs(context.Context, []int64) (map[int64][]*service.ChannelMonitorLatest, error) {
	return nil, nil
}
func (*monitorUsageHandlerRepo) ComputeAvailabilityForMonitors(context.Context, []int64, int) (map[int64][]*service.ChannelMonitorAvailability, error) {
	return nil, nil
}
func (r *monitorUsageHandlerRepo) ResolveUsageGroupsByAPIKeys(_ context.Context, keys []string) (map[string]int64, error) {
	r.calls++
	r.keys = append([]string(nil), keys...)
	if r.fail {
		return nil, errors.New("sensitive query detail")
	}
	return map[string]int64{"private-monitor-key": r.group}, nil
}
func TestChannelMonitorListResolvesCurrentUsageGroupsWithoutLeakingKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &monitorUsageHandlerRepo{group: 246}
	h := NewChannelMonitorHandler(service.NewChannelMonitorService(repo, duplicateChannelMonitorHandlerEncryptor{}))
	router := gin.New()
	router.GET("/monitors", h.List)
	for _, step := range []struct {
		group int64
		fail  bool
	}{{246, false}, {249, false}, {0, false}, {249, true}} {
		repo.group, repo.fail, repo.calls = step.group, step.fail, 0
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/monitors", nil))
		require.Equal(t, http.StatusOK, w.Code)
		var body struct {
			Data struct {
				Items []map[string]any `json:"items"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Len(t, body.Data.Items, 3)
		for i, item := range body.Data.Items {
			value, exists := item["usage_group_id"]
			require.True(t, exists, "missing usage_group_id: new monitors cannot auto-associate")
			if i < 2 && step.group > 0 && !step.fail {
				require.EqualValues(t, step.group, value)
			} else {
				require.Nil(t, value)
			}
		}
		require.Equal(t, 1, repo.calls)
		require.ElementsMatch(t, []string{"private-monitor-key", "external-key"}, repo.keys)
		require.NotContains(t, w.Body.String(), "private-monitor-key")
		require.NotContains(t, w.Body.String(), "external-key")
		require.NotContains(t, w.Body.String(), "sensitive query detail")
	}
}
