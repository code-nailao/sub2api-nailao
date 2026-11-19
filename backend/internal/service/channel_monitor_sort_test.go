//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type monitorOrderRepoStub struct {
	ChannelMonitorRepository
	writes [][]ChannelMonitorSortOrderUpdate
}

func (r *monitorOrderRepoStub) UpdateSortOrders(_ context.Context, updates []ChannelMonitorSortOrderUpdate) error {
	r.writes = append(r.writes, updates)
	return nil
}

func TestChannelMonitorSortRejectsInvalidBatchWithoutWriting(t *testing.T) {
	for _, updates := range [][]ChannelMonitorSortOrderUpdate{
		nil, {{ID: 0}}, {{ID: -1}}, {{ID: 1, SortOrder: -1}},
		{{ID: 1, SortOrder: 2147483648}}, {{ID: 1}, {ID: 1, SortOrder: 10}},
		make([]ChannelMonitorSortOrderUpdate, 10001),
	} {
		repo := &monitorOrderRepoStub{}
		err := NewChannelMonitorService(repo, nil).UpdateSortOrders(context.Background(), updates)
		require.ErrorIs(t, err, ErrChannelMonitorInvalidRequestBody)
		require.Empty(t, repo.writes)
	}
}

func TestChannelMonitorSortPersistsOneBatch(t *testing.T) {
	repo := &monitorOrderRepoStub{}
	updates := []ChannelMonitorSortOrderUpdate{{ID: 9, SortOrder: 0}, {ID: 2, SortOrder: 10}}
	require.NoError(t, NewChannelMonitorService(repo, nil).UpdateSortOrders(context.Background(), updates))
	require.Equal(t, [][]ChannelMonitorSortOrderUpdate{updates}, repo.writes)
}
