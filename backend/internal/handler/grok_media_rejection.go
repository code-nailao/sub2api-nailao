package handler

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Restore only the supplier billing profile captured at create time. Never
// mutate a scheduler-cached account or override credentials/routing settings.
func grokMediaAccountForReservation(account *service.Account, r *service.UpstreamRejectionReservation) *service.Account {
	if account == nil || r == nil || account.ID != r.AccountID {
		return account
	}
	copyAccount := *account
	copyAccount.Extra = make(map[string]any, len(account.Extra)+3)
	for k, v := range account.Extra {
		copyAccount.Extra[k] = v
	}
	copyAccount.Extra["upstream_rejection_profile"] = r.ProviderProfile
	copyAccount.Extra["upstream_rejection_cost"] = r.UpstreamCost
	copyAccount.Extra["upstream_rejection_currency"] = r.UpstreamCurrency
	return &copyAccount
}

func prepareGrokVideoRejectionBilling(result *service.OpenAIForwardResult, r *service.UpstreamRejectionReservation, taskID string) *service.OpenAIForwardResult {
	if result == nil || result.UpstreamRejection == nil || r == nil || r.Task == nil ||
		strings.TrimSpace(taskID) == "" || r.Task.TaskID != strings.TrimSpace(taskID) {
		return nil
	}
	copyResult := *result
	copyResult.UpstreamRejectionReservationID = r.RequestID
	copyResult.RequestID = r.RequestID
	copyResult.ResponseID = r.Task.TaskID
	copyResult.Model = r.Model
	copyResult.BillingModel = r.Model
	copyResult.UpstreamModel = r.Task.UpstreamModel
	return &copyResult
}
