package repository

import (
	"context"
	"database/sql"
	"errors"
	"math"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func sourceImageClaim(ctx context.Context, tx *sql.Tx, id string, key int64, lock bool) (string, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	var fp string
	err := tx.QueryRowContext(ctx, "SELECT request_fingerprint FROM usage_billing_dedup WHERE request_id=$1 AND api_key_id=$2"+suffix, id, key).Scan(&fp)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, "SELECT request_fingerprint FROM usage_billing_dedup_archive WHERE request_id=$1 AND api_key_id=$2"+suffix, id, key).Scan(&fp)
	}
	return fp, err
}
func lockSourcesImageHold(ctx context.Context, tx *sql.Tx, cmd *service.BatchImageBalanceHoldCommand) error {
	if cmd == nil || !service.HasSourcesMediaEngineTaskPrefix(cmd.BatchID) || cmd.UserID <= 0 || cmd.APIKeyID <= 0 || cmd.HoldAmount < 0 || math.IsNaN(cmd.HoldAmount) || math.IsInf(cmd.HoldAmount, 0) {
		return service.ErrUsageBillingRequestConflict
	}
	original := *cmd
	original.RequestID = service.BatchImageHoldRequestID(cmd.BatchID)
	original.ActualAmount = 0
	original.RequestFingerprint = ""
	original.Normalize()
	fingerprint, err := sourceImageClaim(ctx, tx, original.RequestID, cmd.APIKeyID, true)
	if err != nil {
		return err
	}
	if fingerprint != original.RequestFingerprint {
		return service.ErrUsageBillingRequestConflict
	}
	return nil
}
func rejectSourcesImageClaim(ctx context.Context, tx *sql.Tx, id string, key int64) error {
	_, err := sourceImageClaim(ctx, tx, id, key, false)
	if err == nil {
		return service.ErrUsageBillingRequestConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}
func (r *usageBillingRepository) SettleSourcesImage(ctx context.Context, hold *service.BatchImageBalanceHoldCommand, bill *service.UsageBillingCommand) (*service.UsageBillingApplyResult, error) {
	if hold == nil || bill == nil || hold.ActualAmount != hold.HoldAmount || hold.RequestID != service.BatchImageCaptureRequestID(hold.BatchID) || !service.IsSourcesMediaEngineBillingRequestID(bill.RequestID, hold.BatchID) || hold.UserID != bill.UserID || hold.APIKeyID != bill.APIKeyID || bill.BalanceCost != 0 || bill.SubscriptionCost != 0 || bill.SubscriptionID != nil {
		return nil, service.ErrUsageBillingRequestConflict
	}
	hold.Normalize()
	bill.Normalize()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = lockSourcesImageHold(ctx, tx, hold); err != nil {
		return nil, err
	}
	if err = rejectSourcesImageClaim(ctx, tx, service.BatchImageReleaseRequestID(hold.BatchID), hold.APIKeyID); err != nil {
		return nil, err
	}
	capture, err := r.claimUsageBillingRequest(ctx, tx, hold.RequestID, hold.APIKeyID, hold.RequestFingerprint)
	if err != nil {
		return nil, err
	}
	var captured *service.BatchImageBalanceHoldResult
	if capture {
		if captured, err = captureUsageBillingBatchImageBalance(ctx, tx, hold); err != nil {
			return nil, err
		}
	}
	apply, err := r.claimUsageBillingRequest(ctx, tx, bill.RequestID, bill.APIKeyID, bill.RequestFingerprint)
	if err != nil {
		return nil, err
	}
	result := &service.UsageBillingApplyResult{Applied: apply}
	if captured != nil {
		result.NewBalance = captured.NewBalance
	}
	if apply {
		if err = r.applyUsageBillingEffects(ctx, tx, bill, result); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
func (r *usageBillingRepository) ReleaseSourcesImage(ctx context.Context, cmd *service.BatchImageBalanceHoldCommand) error {
	if cmd == nil || cmd.RequestID != service.BatchImageReleaseRequestID(cmd.BatchID) || cmd.ActualAmount != 0 {
		return service.ErrUsageBillingRequestConflict
	}
	cmd.Normalize()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = lockSourcesImageHold(ctx, tx, cmd); err != nil {
		return err
	}
	if err = rejectSourcesImageClaim(ctx, tx, service.BatchImageCaptureRequestID(cmd.BatchID), cmd.APIKeyID); err != nil {
		return err
	}
	apply, err := r.claimUsageBillingRequest(ctx, tx, cmd.RequestID, cmd.APIKeyID, cmd.RequestFingerprint)
	if err != nil {
		return err
	}
	if apply {
		if _, err = releaseUsageBillingBatchImageBalance(ctx, tx, cmd); err != nil {
			return err
		}
	}
	return tx.Commit()
}
