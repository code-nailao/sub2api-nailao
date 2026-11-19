package service

import "context"

// SourcesImageLedger is an optional Main-native extension. Both terminal money
// operations lock the original native hold receipt, so capture and release can
// never consume another task's aggregate frozen funds, even after a stale task
// database restore. No private business table or external balance API is added.
type SourcesImageLedger interface {
	SettleSourcesImage(context.Context, *BatchImageBalanceHoldCommand, *UsageBillingCommand) (*UsageBillingApplyResult, error)
	ReleaseSourcesImage(context.Context, *BatchImageBalanceHoldCommand) error
}
