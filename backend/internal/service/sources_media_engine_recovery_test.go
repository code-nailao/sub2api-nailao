package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sources.local/mediaengine"
)

const sourcesRecoveryPolicyFixture = `{"id":"test-v1","effective_from_unix":1,"timeout_seconds":3600,"max_release_amount":"0.10","max_closures_per_account_24h":3}`

func TestSourcesMediaEngineRecoveryPolicyFile(t *testing.T) {
	policy, err := loadSourcesMediaEngineRecoveryPolicy("")
	require.NoError(t, err)
	require.Nil(t, policy)

	for _, tc := range []struct {
		name string
		raw  string
		mode os.FileMode
		want bool
	}{
		{"valid", sourcesRecoveryPolicyFixture, 0600, true},
		{"readable", sourcesRecoveryPolicyFixture, 0644, true},
		{"unknown_field", strings.TrimSuffix(sourcesRecoveryPolicyFixture, "}") + `,"disable_ledger_checks":true}`, 0600, false},
		{"trailing_document", sourcesRecoveryPolicyFixture + `{}`, 0600, false},
		{"invalid_json", "{", 0600, false},
		{"null", "null", 0600, false},
		{"unsafe_timeout", strings.Replace(sourcesRecoveryPolicyFixture, `:3600`, `:1`, 1), 0600, false},
		{"world_writable", sourcesRecoveryPolicyFixture, 0666, false},
		{"group_writable", sourcesRecoveryPolicyFixture, 0660, false},
		{"oversized", strings.Repeat(" ", 128<<10) + sourcesRecoveryPolicyFixture, 0600, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "recovery.json")
			require.NoError(t, os.WriteFile(path, []byte(tc.raw), 0600))
			require.NoError(t, os.Chmod(path, tc.mode))
			policy, err := loadSourcesMediaEngineRecoveryPolicy(path)
			if tc.want {
				require.NoError(t, err)
				require.Equal(t, "test-v1", policy.ID)
				require.Equal(t, "0.10", policy.MaxReleaseAmount)
			} else {
				require.Error(t, err)
				require.Nil(t, policy)
			}
		})
	}
	_, err = loadSourcesMediaEngineRecoveryPolicy(filepath.Join(t.TempDir(), "absent"))
	require.Error(t, err)
	_, err = loadSourcesMediaEngineRecoveryPolicy(t.TempDir())
	require.Error(t, err)
}

func TestSourcesMediaEngineRecoveryEnableMarker(t *testing.T) {
	const policyID = "test-v1"
	path := filepath.Join(t.TempDir(), "enable")
	require.False(t, sourcesMediaEngineRecoveryEnabled("", policyID))
	require.False(t, sourcesMediaEngineRecoveryEnabled(path, policyID))
	for _, tc := range []struct {
		name string
		raw  string
		mode os.FileMode
		want bool
	}{
		{"exact", policyID, 0600, true},
		{"readable", policyID, 0644, true},
		{"newline", policyID + "\n", 0600, false},
		{"other_policy", "test-v2", 0600, false},
		{"empty", "", 0600, false},
		{"group_writable", policyID, 0660, false},
		{"world_writable", policyID, 0602, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(path, []byte(tc.raw), 0600))
			require.NoError(t, os.Chmod(path, tc.mode))
			require.Equal(t, tc.want, sourcesMediaEngineRecoveryEnabled(path, policyID))
		})
	}
	require.NoError(t, os.Remove(path))
	require.False(t, sourcesMediaEngineRecoveryEnabled(path, policyID))
	require.False(t, sourcesMediaEngineRecoveryEnabled(t.TempDir(), policyID))
}

func sourcesRecoveryMoneyTask(t *testing.T, amount float64) (*mediaengine.Task, sourcesMediaEngineTicket) {
	t.Helper()
	owner := mediaengine.Owner{UserID: 1, KeyID: 2, GroupID: 74}
	group := int64(74)
	ticket := sourcesMediaEngineTicket{Version: 1, Owner: owner, Platform: PlatformOpenAI, AccountID: 713, Amount: amount, Billing: UsageBillingCommand{UserID: 1, APIKeyID: 2, AccountID: 713, AccountType: AccountTypeAPIKey, Model: "gpt-image-2", ImageCount: 1, BillingType: BillingTypeBalance, RequestPayloadHash: strings.Repeat("a", 64)}, Log: UsageLog{UserID: 1, APIKeyID: 2, AccountID: 713, GroupID: &group, Model: "gpt-image-2", ImageCount: 1, BillingType: BillingTypeBalance, ActualCost: amount, TotalCost: amount, RateMultiplier: 1}}
	ticket.Billing.Normalize()
	raw, err := json.Marshal(ticket)
	require.NoError(t, err)
	return &mediaengine.Task{ID: "mediatask_" + strings.Repeat("b", 32), Owner: owner, AccountID: 713, PayloadHash: strings.Repeat("a", 64), Ticket: raw}, ticket
}

func TestSourcesMediaEngineRecoveryUsesOnlyOriginalAmount(t *testing.T) {
	s := &SourcesMediaEngine{}
	for _, tc := range []struct {
		amount float64
		minor  int64
	}{
		{0, 0}, {.00000001, 1}, {.03, 3000000}, {.06, 6000000}, {100.12345678, 10012345678},
	} {
		task, _ := sourcesRecoveryMoneyTask(t, tc.amount)
		amount, err := s.UnknownReleaseAmount(task)
		require.NoError(t, err)
		require.Equal(t, tc.minor, amount)
	}
	_, err := s.UnknownReleaseAmount(nil)
	require.ErrorIs(t, err, mediaengine.ErrConflict)
	task, ticket := sourcesRecoveryMoneyTask(t, .06)
	ticket.Amount = .6
	task.Ticket, err = json.Marshal(ticket)
	require.NoError(t, err)
	_, err = s.UnknownReleaseAmount(task)
	require.ErrorIs(t, err, mediaengine.ErrConflict)
	task, _ = sourcesRecoveryMoneyTask(t, .000000001)
	_, err = s.UnknownReleaseAmount(task)
	require.ErrorIs(t, err, mediaengine.ErrConflict)
	task, _ = sourcesRecoveryMoneyTask(t, .06)
	task.Owner.UserID++
	_, err = s.UnknownReleaseAmount(task)
	require.ErrorIs(t, err, mediaengine.ErrConflict)
}

type sourcesRecoveryLedger struct {
	UsageBillingRepository
	SourcesImageLedger
	err   error
	holds []*BatchImageBalanceHoldCommand
}

func (r *sourcesRecoveryLedger) ReleaseSourcesImage(_ context.Context, hold *BatchImageBalanceHoldCommand) error {
	r.holds = append(r.holds, hold)
	return r.err
}

func TestSourcesMediaEngineRecoveryReleasePreservesLedgerGuards(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		want  error
	}{
		{"captured_or_fingerprint_conflict", ErrUsageBillingRequestConflict, mediaengine.ErrFinancialConflict},
		{"original_hold_missing", sql.ErrNoRows, mediaengine.ErrFinancialConflict},
		{"transient", context.DeadlineExceeded, context.DeadlineExceeded},
		{"success", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := &sourcesRecoveryLedger{err: tc.cause}
			s := &SourcesMediaEngine{gateway: &OpenAIGatewayService{usageBillingRepo: ledger}}
			task, _ := sourcesRecoveryMoneyTask(t, .06)
			err := s.Release(context.Background(), task)
			require.True(t, errors.Is(err, tc.want), "unexpected release classification: %v", err)
			require.Len(t, ledger.holds, 1)
			require.Equal(t, .06, ledger.holds[0].HoldAmount)
			require.Zero(t, ledger.holds[0].ActualAmount)
			require.Equal(t, task.PayloadHash, ledger.holds[0].RequestPayloadHash)
			require.Equal(t, BatchImageReleaseRequestID(task.ID), ledger.holds[0].RequestID)
		})
	}
}

func TestSourcesMediaEngineRecoverySummaryWithoutConfiguredEngine(t *testing.T) {
	var s *SourcesMediaEngine
	require.Equal(t, "disabled", s.RecoverySummary(nil).Status)
	s = &SourcesMediaEngine{}
	require.True(t, s.RecoverySummary(&mediaengine.Task{Phase: "unknown"}).ManualActionRequired)
}
