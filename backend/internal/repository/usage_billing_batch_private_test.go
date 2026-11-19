//go:build unit

package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestUsageBillingBatchPrivateEffectsRejectedBeforeMutation(t *testing.T) {
	cases := []struct {
		name string
		set  func(*service.UsageBillingCommand)
	}{
		{"rejection hold", func(c *service.UsageBillingCommand) { c.UpstreamRejectionReservationID = "hold" }},
		{"tool fingerprint", func(c *service.UsageBillingCommand) { c.DeepSeekToolFingerprint = "tool" }},
		{"tool account", func(c *service.UsageBillingCommand) { c.ToolAccountID = 3 }},
		{"tool cost", func(c *service.UsageBillingCommand) { c.ToolAccountQuotaCost = -1e-12 }},
		{"tool daily", func(c *service.UsageBillingCommand) { c.ToolAccountDailyQuota = true }},
		{"tool weekly", func(c *service.UsageBillingCommand) { c.ToolAccountWeeklyQuota = true }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			repo := &usageBillingRepository{db: db}
			first := &service.UsageBillingCommand{RequestID: " ordinary ", UserID: 1, APIKeyID: 2, BalanceCost: 0.1234567891}
			special := &service.UsageBillingCommand{RequestID: " special ", UserID: 1, APIKeyID: 2, BalanceCost: -1e-12}
			tt.set(special)
			firstBefore, specialBefore := *first, *special
			result, err := repo.ApplyBatch(context.Background(), []*service.UsageBillingCommand{first, special})
			require.Nil(t, result)
			require.ErrorIs(t, err, errUsageBillingBatchPrivateEffects)
			require.Equal(t, firstBefore, *first, "earlier ordinary command must not be normalized on rejected batch")
			require.Equal(t, specialBefore, *special, "private validation must see the original raw command")
			require.NoError(t, mock.ExpectationsWereMet(), "no transaction or money effects may occur")
		})
	}
}
