package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentauditlog"
	"github.com/Wei-Shaw/sub2api/ent/paymentorder"
)

const paymentPostCommitPending = "PAYMENT_POST_COMMIT_PENDING"
const paymentPostCommitDone = "PAYMENT_POST_COMMIT_DONE"

type paymentPostCommitJob struct {
	Action     string    `json:"action"`
	Attempt    int       `json:"attempt"`
	RetryAfter time.Time `json:"retry_after"`
}

// Stored in the same transaction as completion; audit's (order_id, action)
// uniqueness is the durable job key. CAS detail is a bounded retry lease.
func queuePaymentPostCommit(ctx context.Context, client *dbent.Client, o *dbent.PaymentOrder, action string) error {
	done, err := client.PaymentAuditLog.Query().Where(paymentauditlog.OrderIDEQ(fmt.Sprint(o.ID)), paymentauditlog.ActionEQ(paymentPostCommitDone)).Exist(ctx)
	if err != nil || done {
		return err
	}
	detail, _ := json.Marshal(paymentPostCommitJob{Action: action})
	return paymentAuditOnce(ctx, client, o.ID, paymentPostCommitPending, json.RawMessage(detail))
}

func (s *PaymentService) processPaymentPostCommit(ctx context.Context, orderID int64) error {
	job, err := s.entClient.PaymentAuditLog.Query().Where(paymentauditlog.OrderIDEQ(strconv.FormatInt(orderID, 10)), paymentauditlog.ActionEQ(paymentPostCommitPending)).Only(ctx)
	if dbent.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var payload paymentPostCommitJob
	if err = json.Unmarshal([]byte(job.Detail), &payload); err != nil {
		return err
	}
	if payload.RetryAfter.After(time.Now()) {
		return nil
	}
	payload.Attempt++
	payload.RetryAfter = time.Now().UTC().Add(2 * time.Minute)
	claim, _ := json.Marshal(payload)
	n, err := s.entClient.PaymentAuditLog.Update().Where(paymentauditlog.IDEQ(job.ID), paymentauditlog.ActionEQ(paymentPostCommitPending), paymentauditlog.DetailEQ(job.Detail)).SetDetail(string(claim)).Save(ctx)
	if err != nil || n == 0 {
		return err
	}
	// The batch budget controls admission, not an already claimed delivery.
	jobCtx, jobCancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer jobCancel()
	ctx = jobCtx
	o, err := s.entClient.PaymentOrder.Get(ctx, orderID)
	if err != nil {
		return err
	}
	if o.Status != OrderStatusCompleted && !psIsRefundStatus(o.Status) {
		return fmt.Errorf("payment post-commit order is not complete")
	}
	if payload.Action == "RECHARGE_SUCCESS" {
		// Balance-only writes do not fire the existing user auth trigger.
		// Reuse its durable queue so a lost best-effort invalidation is repaired.
		if paymentAuditDialect(s.entClient) == dialect.Postgres {
			if _, err = s.entClient.ExecContext(ctx, "SELECT enqueue_auth_cache_invalidation(key) FROM api_keys WHERE user_id = $1", o.UserID); err != nil {
				return err
			}
		}
		if s.redeemService != nil {
			if s.redeemService.billingCacheService != nil {
				if err = s.redeemService.billingCacheService.InvalidateUserBalance(ctx, o.UserID); err != nil {
					return err
				}
			}
			if s.redeemService.authCacheInvalidator != nil {
				s.redeemService.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, o.UserID)
			}
		}
	}
	if s.subscriptionSvc != nil && o.SubscriptionGroupID != nil {
		if err = s.subscriptionSvc.invalidateSubscriptionCaches(o.UserID, *o.SubscriptionGroupID); err != nil {
			return err
		}

	}

	if s.notificationEmailService != nil {
		switch payload.Action {
		case "RECHARGE_SUCCESS":
			err = s.sendBalanceRechargeSuccessNotification(ctx, o)
		case "SUBSCRIPTION_SUCCESS":
			err = s.sendSubscriptionPurchaseSuccessNotification(ctx, o)
		}
		if err != nil {
			return err
		}
	}
	ackCtx, ackCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer ackCancel()
	_, err = s.entClient.PaymentAuditLog.Update().Where(paymentauditlog.IDEQ(job.ID), paymentauditlog.ActionEQ(paymentPostCommitPending), paymentauditlog.DetailEQ(string(claim))).SetAction(paymentPostCommitDone).Save(ackCtx)
	return err
}

// One bounded worker sweep for all providers. A crash after receipt commit is
// recoverable without a new callback; a receipt prevents repeated remote queries.
func (s *PaymentService) reconcileVerifiedPayments(ctx context.Context) error {
	orders, err := s.entClient.PaymentOrder.Query().Where(paymentorder.Or(paymentorder.ProviderKeyIn("bepusdt", "easypay"), func(selector *sql.Selector) {
		selector.Where(sql.In(selector.C(paymentorder.FieldID), sql.Select("order_id").From(sql.Table("payment_receipts"))))
	}), paymentorder.PaidAtNotNil(), paymentorder.Or(paymentorder.StatusIn(OrderStatusPaid, OrderStatusFailed), paymentorder.And(paymentorder.StatusEQ(OrderStatusRecharging), paymentorder.UpdatedAtLTE(time.Now().Add(-paymentFulfillmentLeaseDuration))))).Order(dbent.Asc(paymentorder.FieldUpdatedAt)).Limit(20).All(ctx)
	if err != nil {
		return err
	}
	for _, o := range orders {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.executeFulfillment(ctx, o.ID); err != nil {
			_, _ = s.entClient.PaymentOrder.Update().Where(paymentorder.IDEQ(o.ID), paymentorder.UpdatedAtEQ(o.UpdatedAt), paymentorder.StatusIn(OrderStatusPaid, OrderStatusFailed, OrderStatusRecharging)).SetUpdatedAt(time.Now().UTC().Truncate(time.Microsecond)).Save(ctx)
		}
	}
	return nil
}

func (s *PaymentService) reconcilePaymentPostCommit(ctx context.Context) error {
	jobs, err := s.entClient.PaymentAuditLog.Query().Where(paymentauditlog.ActionEQ(paymentPostCommitPending), func(selector *sql.Selector) {
		if paymentAuditDialect(s.entClient) == dialect.Postgres {
			selector.Where(sql.ExprP("(detail::jsonb->>'retry_after')::timestamptz <= CURRENT_TIMESTAMP"))
		} else {
			selector.Where(sql.ExprP("json_extract(detail, '$.retry_after') <= ?", time.Now().UTC().Format(time.RFC3339Nano)))
		}
	}).Order(dbent.Asc(paymentauditlog.FieldID)).Limit(100).All(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		id, err := strconv.ParseInt(job.OrderID, 10, 64)
		if err != nil {
			return err
		}
		_ = s.processPaymentPostCommit(ctx, id)
	}
	return nil
}
