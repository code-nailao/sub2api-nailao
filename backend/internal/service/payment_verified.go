package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"entgo.io/ent/dialect"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentauditlog"
	"github.com/Wei-Shaw/sub2api/ent/paymentorder"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/payment/proof"
	"github.com/Wei-Shaw/sub2api/internal/payment/provider"
	"github.com/shopspring/decimal"
)

const OrderStatusPaymentReview = "PAYMENT_REVIEW"
const paymentVerificationPending = "payment_verification_pending"

// These two adapters provide authoritative exact-money/identity contracts.
// This boundary never treats a callback, redirect, or UI status as payment proof.
func verifiedPaymentProvider(key string) bool {
	return key == payment.TypeBEPusdt || key == payment.TypeEasyPay
}
func verifiedPaymentOrder(o *dbent.PaymentOrder) bool {
	return o != nil && verifiedPaymentProvider(psStringValue(o.ProviderKey))
}
func nativeUSDTOrder(o *dbent.PaymentOrder) bool {
	return o != nil && psStringValue(o.ProviderKey) == payment.TypeBEPusdt
}

func (s *PaymentService) pinVerifiedProvider(ctx context.Context, client *dbent.Client, sel *payment.InstanceSelection, snapshot map[string]any) error {
	if sel == nil || !verifiedPaymentProvider(sel.ProviderKey) {
		return nil
	}
	id, err := strconv.ParseInt(sel.InstanceID, 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("payment provider instance required")
	}
	cfg := make(map[string]string, len(sel.Config)+1)
	for k, v := range sel.Config {
		cfg[k] = v
	}
	cfg["paymentMode"] = sel.PaymentMode
	version, ns, err := proof.Pin(ctx, client, id, sel.ProviderKey, cfg)
	if err != nil {
		return err
	}
	snapshot["provider_version_id"], snapshot["payment_namespace"] = strconv.FormatInt(version, 10), ns
	snapshot["notify_url"], snapshot["return_url"] = cfg["notifyUrl"], cfg["returnUrl"]
	if sel.ProviderKey == payment.TypeBEPusdt {
		addresses, err := payment.ParseBEPusdtWalletAddresses(cfg["walletAddresses"])
		if err != nil {
			return err
		}
		snapshot["crypto_requested_addresses"] = addresses
	}
	return nil
}

func (s *PaymentService) resolveVerifiedProvider(ctx context.Context, o *dbent.PaymentOrder) (payment.Provider, bool, error) {
	raw := psSnapshotStringValue(o.ProviderSnapshot["provider_version_id"])
	if raw == "" {
		return nil, false, nil
	}
	vid, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, true, err
	}
	iid, err := strconv.ParseInt(psStringValue(o.ProviderInstanceID), 10, 64)
	if err != nil {
		return nil, true, err
	}
	cfg, err := proof.Resolve(ctx, s.entClient, vid, iid, psStringValue(o.ProviderKey), psSnapshotStringValue(o.ProviderSnapshot["payment_namespace"]))
	if err != nil {
		return nil, true, err
	}
	p, err := provider.CreateProvider(psStringValue(o.ProviderKey), psStringValue(o.ProviderInstanceID), cfg)
	return p, true, err
}

func verifiedReturnURL(sel *payment.InstanceSelection) (string, error) {
	u, err := url.Parse(sel.Config["returnUrl"])
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("configured HTTPS payment return URL required")
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment = "/payment/result", "", "", ""
	n, err := url.Parse(sel.Config["notifyUrl"])
	if err != nil || n.Scheme != "https" || n.Host == "" || n.User != nil || n.Path != "/api/v1/payment/webhook/"+sel.ProviderKey || n.RawQuery != "" || n.Fragment != "" {
		return "", fmt.Errorf("configured provider notification URL required")
	}
	return u.String(), nil
}

func cryptoSnapshot(o *dbent.PaymentOrder) *payment.CryptoPaymentDetails {
	var result payment.CryptoPaymentDetails
	data, err := json.Marshal(o.ProviderSnapshot["crypto_payment"])
	if err != nil || string(data) == "null" || json.Unmarshal(data, &result) != nil {
		return nil
	}
	return &result
}

func validateNativePayment(o *dbent.PaymentOrder, q *payment.CryptoPaymentDetails) error {
	n, ok := payment.BEPusdtNetworkByMethod(o.PaymentType)
	if !ok || q == nil || q.Asset != "USDT" || q.Fiat != "CNY" || q.TradeType != n.TradeType || q.Network != n.Network || o.PaymentTradeNo != "" && q.TradeID != o.PaymentTradeNo {
		return proof.ErrConflict
	}
	if _, ok := n.CanonicalAddress(q.Address); !ok {
		return proof.ErrConflict
	}
	amount, err := decimal.NewFromString(q.FiatAmount)
	if err != nil || !amount.Equal(decimal.NewFromFloat(o.PayAmount)) {
		return proof.ErrConflict
	}
	if fixed := cryptoSnapshot(o); fixed != nil && fixed.ChainAmount != "" {
		if fixed.ChainAmount != q.ChainAmount || fixed.TradeType != q.TradeType || !n.SameAddress(fixed.Address, q.Address) {
			return proof.ErrConflict
		}
	} else {
		requested := psSnapshotStringValue(o.ProviderSnapshot["crypto_requested_address"])
		if requested == "" {
			data, _ := json.Marshal(o.ProviderSnapshot["crypto_requested_addresses"])
			addresses := map[string]string{}
			_ = json.Unmarshal(data, &addresses)
			requested = addresses[o.PaymentType]
		}
		if requested != "" && !n.SameAddress(requested, q.Address) {
			return proof.ErrConflict
		}
	}
	return nil
}

func validateNativeProviderConfig(cfg map[string]string, types []string) error {
	if len(types) == 0 {
		return fmt.Errorf("select explicit USDT networks")
	}
	for _, method := range types {
		if _, ok := payment.BEPusdtNetworkByMethod(method); !ok {
			return fmt.Errorf("unknown USDT network")
		}
	}
	_, err := provider.NewBEPusdt("validate", cfg)
	return err
}

func (s *PaymentService) storeNativePaymentCreation(ctx context.Context, o *dbent.PaymentOrder, result *payment.CreatePaymentResponse) error {
	if result == nil || result.CryptoPayment == nil || result.TradeNo != result.CryptoPayment.TradeID {
		return proof.ErrConflict
	}
	if err := validateNativePayment(o, result.CryptoPayment); err != nil {
		return err
	}
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	query := tx.PaymentOrder.Query().Where(paymentorder.IDEQ(o.ID))
	if paymentAuditDialect(s.entClient) == dialect.Postgres {
		query.ForUpdate()
	}
	fresh, err := query.Only(ctx)
	if err != nil {
		return err
	}
	if fresh.UserID != o.UserID || fresh.OutTradeNo != o.OutTradeNo || fresh.PayAmount != o.PayAmount || fresh.PaymentType != o.PaymentType {
		return proof.ErrConflict
	}
	if err := validateNativePayment(fresh, result.CryptoPayment); err != nil {
		return err
	}
	if fresh.PaidAt != nil || fresh.Status == OrderStatusCompleted {
		return tx.Commit()
	}
	if fresh.Status != OrderStatusPending {
		return fmt.Errorf("native checkout state changed; verify original order")
	}
	snapshot := make(map[string]any, len(fresh.ProviderSnapshot)+1)
	for k, v := range fresh.ProviderSnapshot {
		snapshot[k] = v
	}
	details := *result.CryptoPayment
	details.ExpiresAt = fresh.ExpiresAt
	snapshot["crypto_payment"] = &details
	if _, err = tx.PaymentOrder.UpdateOneID(o.ID).SetProviderSnapshot(snapshot).SetPaymentTradeNo(result.TradeNo).SetPayURL(result.PayURL).Save(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PaymentService) paymentEvidence(ctx context.Context, o *dbent.PaymentOrder, p payment.Provider, q *payment.QueryOrderResponse) (*proof.Receipt, error) {
	if q == nil || q.Status != payment.ProviderStatusPaid || q.TradeNo == "" || q.OrderID != paymentBusinessOrderID(o) || q.Currency != PaymentOrderCurrency(o) {
		return nil, fmt.Errorf("payment has not been authoritatively confirmed")
	}
	if o.PaymentTradeNo != "" && q.TradeNo != o.PaymentTradeNo {
		return nil, proof.ErrConflict
	}
	amount, err := decimal.NewFromString(q.AmountExact)
	if err != nil || !amount.IsPositive() || !amount.Equal(decimal.NewFromFloat(o.PayAmount)) {
		return nil, proof.ErrConflict
	}
	if p.ProviderKey() != psStringValue(o.ProviderKey) {
		return nil, proof.ErrConflict
	}
	if p.ProviderKey() == payment.TypeEasyPay {
		matcher, ok := p.(payment.PaymentMethodMatcher)
		if !ok || q.PaymentType == "" || !matcher.MatchesPaymentMethod(o.PaymentType, q.PaymentType) {
			return nil, proof.ErrConflict
		}
	}
	if err := validateProviderNotificationMetadata(o, p.ProviderKey(), q.Metadata); err != nil {
		return nil, err
	}
	ns := psSnapshotStringValue(o.ProviderSnapshot["payment_namespace"])
	if ns == "" {
		inst, err := s.getOrderProviderInstance(ctx, o)
		if err != nil {
			return nil, err
		}
		if inst == nil {
			return nil, proof.ErrConflict
		}
		cfg, err := s.loadBalancer.GetInstanceConfig(ctx, inst.ID)
		if err != nil {
			return nil, err
		}
		ns = proof.Namespace(p.ProviderKey(), cfg)
	}
	r := &proof.Receipt{OrderID: o.ID, Namespace: ns, TradeID: q.TradeNo, ProviderKey: p.ProviderKey(), PaymentType: o.PaymentType, Currency: q.Currency, Amount: amount.String(), Evidence: map[string]string{"order_id": q.OrderID, "trade_id": q.TradeNo, "currency": q.Currency, "amount": amount.String()}}
	if nativeUSDTOrder(o) {
		if err := validateNativePayment(o, q.CryptoPayment); err != nil {
			return nil, err
		}
		c := q.CryptoPayment
		n, _ := payment.BEPusdtNetworkByMethod(o.PaymentType)
		if _, ok := n.CanonicalHash(c.TransactionHash); !ok {
			return nil, proof.ErrConflict
		}
		r.ChainIdentity = proof.Digest(c.Network, c.Asset, c.TransactionHash)
		for k, v := range map[string]string{"network": c.Network, "asset": c.Asset, "chain_amount": c.ChainAmount, "address": c.Address, "transaction_hash": c.TransactionHash} {
			r.Evidence[k] = v
		}
	}
	return r, nil
}

func paymentAuditOnce(ctx context.Context, c *dbent.Client, id int64, action string, detail any) error {
	exists, err := c.PaymentAuditLog.Query().Where(paymentauditlog.OrderIDEQ(fmt.Sprint(id)), paymentauditlog.ActionEQ(action)).Exist(ctx)
	if err != nil || exists {
		return err
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = c.PaymentAuditLog.Create().SetOrderID(fmt.Sprint(id)).SetAction(action).SetOperator("payment-verifier").SetDetail(string(encoded)).Save(ctx)
	return err
}

// recordPaymentEvidence is the only durable payment-confirmation boundary.
// It does not call fulfillment: recovery may need to repair a receipt first.
func (s *PaymentService) recordPaymentEvidence(ctx context.Context, o *dbent.PaymentOrder, p payment.Provider, q *payment.QueryOrderResponse, approved bool) error {
	r, err := s.paymentEvidence(ctx, o, p, q)
	if err != nil {
		return err
	}
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	query := tx.PaymentOrder.Query().Where(paymentorder.IDEQ(o.ID))
	if paymentAuditDialect(s.entClient) == dialect.Postgres {
		query.ForUpdate()
	}
	fresh, err := query.Only(ctx)
	if err != nil {
		return err
	}
	if fresh.UserID != o.UserID || fresh.OutTradeNo != o.OutTradeNo || fresh.PayAmount != o.PayAmount || fresh.Amount != o.Amount || fresh.PaymentType != o.PaymentType || fresh.PaymentTradeNo != o.PaymentTradeNo {
		return proof.ErrConflict
	}
	if psIsRefundStatus(fresh.Status) {
		return fmt.Errorf("refund-related order requires reconciliation")
	}
	if err := proof.Save(ctx, tx.Client(), r); err != nil {
		return err
	}
	if fresh.PaymentTradeNo == "" {
		if _, err = tx.PaymentOrder.UpdateOneID(o.ID).SetPaymentTradeNo(q.TradeNo).Save(ctx); err != nil {
			return err
		}
	}
	if fresh.Status == OrderStatusCompleted {
		return tx.Commit()
	}
	if !approved && (fresh.Status == OrderStatusPaymentReview || fresh.PaidAt == nil && time.Now().After(fresh.ExpiresAt.Add(paymentGraceMinutes*time.Minute))) {
		_, err = tx.PaymentOrder.UpdateOneID(o.ID).SetStatus(OrderStatusPaymentReview).SetFailedReason("PAYMENT_REQUIRES_REVIEW").Save(ctx)
		if err != nil {
			return err
		}
		if err := paymentAuditOnce(ctx, tx.Client(), o.ID, "PAYMENT_VERIFIED_REVIEW", r.Evidence); err != nil {
			return err
		}
		return tx.Commit()
	}
	if fresh.Status != OrderStatusRecharging {
		b := tx.PaymentOrder.UpdateOneID(o.ID).SetStatus(OrderStatusPaid).SetPaymentTradeNo(q.TradeNo).ClearFailedAt().ClearFailedReason()
		if fresh.PaidAt == nil {
			b.SetPaidAt(time.Now())
		}
		if _, err = b.Save(ctx); err != nil {
			return err
		}
	}
	if err := paymentAuditOnce(ctx, tx.Client(), o.ID, "ORDER_PAID", r.Evidence); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PaymentService) acceptPaymentEvidence(ctx context.Context, o *dbent.PaymentOrder, p payment.Provider, q *payment.QueryOrderResponse, approved bool) error {
	if err := s.recordPaymentEvidence(ctx, o, p, q, approved); err != nil {
		return err
	}
	fresh, err := s.entClient.PaymentOrder.Get(ctx, o.ID)
	if err != nil {
		return err
	}
	if fresh.Status == OrderStatusPaymentReview || fresh.Status == OrderStatusCompleted {
		return nil
	}
	return s.executeFulfillment(ctx, fresh.ID)
}

func (s *PaymentService) confirmVerifiedNotification(ctx context.Context, o *dbent.PaymentOrder, tradeNo string, metadata map[string]string) error {
	p, err := s.getOrderProvider(ctx, o)
	if err != nil {
		return err
	}
	ref := paymentOrderQueryReference(o, p)
	if nativeUSDTOrder(o) && o.PaymentTradeNo == "" {
		ref = tradeNo
	}
	if ref == "" {
		return fmt.Errorf("payment creation outcome requires reconciliation")
	}
	q, err := p.QueryOrder(ctx, ref)
	if err != nil {
		return err
	}
	if q == nil || q.TradeNo != tradeNo || metadata["payment_type"] != "" && metadata["payment_type"] != q.PaymentType {
		return proof.ErrConflict
	}
	return s.acceptPaymentEvidence(ctx, o, p, q, false)
}

func (s *PaymentService) requirePaymentReceipt(ctx context.Context, o *dbent.PaymentOrder) error {
	if !verifiedPaymentOrder(o) {
		return nil
	}
	r, err := proof.Load(ctx, s.entClient, o.ID)
	if errors.Is(err, sql.ErrNoRows) {
		// Old PAID/FAILED/RECHARGING rows are not proof. Query the immutable
		// provider identity, verify it, then repair the receipt transactionally.
		p, e := s.getOrderProvider(ctx, o)
		if e != nil {
			return e
		}
		ref := paymentOrderQueryReference(o, p)
		if ref == "" || nativeUSDTOrder(o) && o.PaymentTradeNo == "" {
			return payment.ErrPaymentOutcomeUnknown
		}
		q, e := p.QueryOrder(ctx, ref)
		if e != nil {
			return e
		}
		if e = s.recordPaymentEvidence(ctx, o, p, q, false); e != nil {
			return e
		}
		fresh, e := s.entClient.PaymentOrder.Get(ctx, o.ID)
		if e != nil {
			return e
		}
		if fresh.Status == OrderStatusPaymentReview {
			return fmt.Errorf("payment requires administrator review")
		}
		*o = *fresh
		r, err = proof.Load(ctx, s.entClient, o.ID)
	}
	if err != nil {
		return fmt.Errorf("verified payment receipt required: %w", err)
	}
	if r.ProviderKey != psStringValue(o.ProviderKey) || r.TradeID != o.PaymentTradeNo || r.PaymentType != o.PaymentType || r.Currency != PaymentOrderCurrency(o) || r.Amount != decimal.NewFromFloat(o.PayAmount).String() || r.Evidence["order_id"] != paymentBusinessOrderID(o) {
		return proof.ErrConflict
	}
	return nil
}

func (s *PaymentService) completeVerifiedOrder(ctx context.Context, o *dbent.PaymentOrder, lease *paymentFulfillmentLease, action string) error {
	if lease == nil {
		return fmt.Errorf("payment fulfillment lease required")
	}
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	n, err := tx.PaymentOrder.Update().Where(paymentorder.IDEQ(o.ID), paymentorder.StatusEQ(OrderStatusRecharging), paymentorder.UpdatedAtEQ(lease.version)).SetStatus(OrderStatusCompleted).SetCompletedAt(time.Now()).Save(ctx)
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("payment fulfillment lease changed")
	}
	if err := paymentAuditOnce(ctx, tx.Client(), o.ID, action, map[string]any{"creditedAmount": o.Amount, "payAmount": o.PayAmount}); err != nil {
		return err
	}
	if err := queuePaymentPostCommit(ctx, tx.Client(), o, action); err != nil {
		return err
	}
	return tx.Commit()
}

// Reconciliation is bounded and owned by the upstream payment worker. Unknown
// submissions are never POSTed again, and PAID/FAILED work requires a receipt.
func (s *PaymentService) reconcileVerifiedPaymentOrders(ctx context.Context) (int, error) {
	if err := s.reviewExpiredUnknownNativeOrders(ctx); err != nil {
		return 0, err
	}
	orders, err := s.entClient.PaymentOrder.Query().Where(paymentorder.ProviderKeyIn(payment.TypeBEPusdt, payment.TypeEasyPay), paymentorder.PaidAtIsNil(), paymentorder.Or(paymentorder.ProviderKeyEQ(payment.TypeEasyPay), paymentorder.PaymentTradeNoNEQ("")), paymentorder.StatusIn(OrderStatusPending, OrderStatusFailed, OrderStatusExpired, OrderStatusCancelled), paymentorder.CreatedAtGTE(time.Now().Add(-7*24*time.Hour))).Order(dbent.Asc(paymentorder.FieldUpdatedAt)).Limit(20).All(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range orders {
		if ctx.Err() != nil {
			break
		}
		r, e := proof.Load(ctx, s.entClient, o.ID)
		if e == nil && r != nil {
			if o.Status != OrderStatusPaymentReview && s.executeFulfillment(ctx, o.ID) == nil {
				n++
			}
			continue
		}
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return n, e
		}
		if s.checkVerifiedPayment(ctx, o, false) == checkPaidResultAlreadyPaid {
			n++
		}
		// Rotate unconfirmed work without touching a claimed fulfillment lease.
		_, _ = s.entClient.PaymentOrder.Update().Where(paymentorder.IDEQ(o.ID), paymentorder.UpdatedAtEQ(o.UpdatedAt), paymentorder.PaidAtIsNil(), paymentorder.StatusEQ(o.Status)).SetUpdatedAt(time.Now()).Save(ctx)
	}
	return n, nil
}

func (s *PaymentService) checkVerifiedPayment(ctx context.Context, o *dbent.PaymentOrder, cancel bool) string {
	p, err := s.getOrderProvider(ctx, o)
	if err != nil {
		return paymentVerificationPending
	}
	ref := paymentOrderQueryReference(o, p)
	if nativeUSDTOrder(o) && o.PaymentTradeNo == "" {
		return paymentVerificationPending
	}
	q, err := p.QueryOrder(ctx, ref)
	if err != nil || q == nil {
		return paymentVerificationPending
	}
	if q.Status == payment.ProviderStatusPaid {
		if s.acceptPaymentEvidence(ctx, o, p, q, false) != nil {
			return paymentVerificationPending
		}
		return checkPaidResultAlreadyPaid
	}
	if nativeUSDTOrder(o) {
		if validateNativePayment(o, q.CryptoPayment) != nil || q.Status == payment.ProviderStatusConfirming {
			return paymentVerificationPending
		}
		if cancel && q.Status == payment.ProviderStatusPending {
			cp, ok := p.(payment.CancelableProvider)
			if !ok || cp.CancelPayment(ctx, ref) != nil {
				return paymentVerificationPending
			}
			after, err := p.QueryOrder(ctx, ref)
			if err != nil || after == nil {
				return paymentVerificationPending
			}
			if after.Status == payment.ProviderStatusPaid {
				if s.acceptPaymentEvidence(ctx, o, p, after, false) != nil {
					return paymentVerificationPending
				}
				return checkPaidResultAlreadyPaid
			}
			if validateNativePayment(o, after.CryptoPayment) != nil || (after.Status != "cancelled" && after.Status != "expired" && after.Status != payment.ProviderStatusFailed) {
				return paymentVerificationPending
			}
			return ""
		}
		if q.Status == payment.ProviderStatusPending && cancel {
			return paymentVerificationPending
		}
	}
	return ""
}

// The business reference is random for modern orders; only genuinely empty
// legacy rows use the old numeric alias. Never accept an alias for a modern row.
func paymentBusinessOrderID(o *dbent.PaymentOrder) string {
	if o.OutTradeNo != "" {
		return o.OutTradeNo
	}
	return orderIDPrefix + strconv.FormatInt(o.ID, 10)
}

// ReconcileNativePayment binds an unknown creation outcome using authoritative
// gateway data. The administrator supplies only a reference, never an amount.
// Paid results still go through the same evidence transaction and review gate.
func (s *PaymentService) ReconcileNativePayment(ctx context.Context, id int64, tradeID string) error {
	o, err := s.entClient.PaymentOrder.Get(ctx, id)
	if err != nil {
		return err
	}
	if !nativeUSDTOrder(o) || psIsRefundStatus(o.Status) {
		return proof.ErrConflict
	}
	if o.PaymentTradeNo != "" && o.PaymentTradeNo != tradeID {
		return proof.ErrConflict
	}
	p, err := s.getOrderProvider(ctx, o)
	if err != nil {
		return err
	}
	q, err := p.QueryOrder(ctx, tradeID)
	if err != nil {
		return err
	}
	if q == nil || q.OrderID != paymentBusinessOrderID(o) || q.TradeNo != tradeID {
		return proof.ErrConflict
	}
	if err = validateNativePayment(o, q.CryptoPayment); err != nil {
		return err
	}
	if q.Status == payment.ProviderStatusPaid {
		return s.acceptPaymentEvidence(ctx, o, p, q, false)
	}
	if q.Status != payment.ProviderStatusPending || o.Status != OrderStatusPending {
		return payment.ErrPaymentOutcomeUnknown
	}
	if err = s.storeNativePaymentCreation(ctx, o, &payment.CreatePaymentResponse{TradeNo: q.TradeNo, PayURL: q.PayURL, CryptoPayment: q.CryptoPayment}); err != nil {
		return err
	}
	s.writeAuditLog(ctx, id, "USDT_ADMIN_RECONCILED", "admin", map[string]any{"trade_id": tradeID, "gateway_status": q.Status})
	return nil
}

// A missing gateway trade ID cannot prove payment or cancellation. Preserve the
// original order for investigation, without occupying normal pending slots forever.
func (s *PaymentService) reviewExpiredUnknownNativeOrders(ctx context.Context) error {
	rows, err := s.entClient.PaymentOrder.Query().Where(paymentorder.ProviderKeyEQ(payment.TypeBEPusdt), paymentorder.StatusEQ(OrderStatusPending), paymentorder.PaidAtIsNil(), paymentorder.PaymentTradeNoEQ(""), paymentorder.ExpiresAtLT(time.Now())).Order(dbent.Asc(paymentorder.FieldID)).Limit(20).All(ctx)
	if err != nil {
		return err
	}
	for _, o := range rows {
		tx, err := s.entClient.Tx(ctx)
		if err != nil {
			return err
		}
		n, err := tx.PaymentOrder.Update().Where(paymentorder.IDEQ(o.ID), paymentorder.StatusEQ(OrderStatusPending), paymentorder.PaidAtIsNil(), paymentorder.PaymentTradeNoEQ("")).SetStatus(OrderStatusPaymentReview).SetFailedReason("PAYMENT_CREATION_OUTCOME_UNKNOWN").Save(ctx)
		if err == nil && n > 0 {
			err = paymentAuditOnce(ctx, tx.Client(), o.ID, "PAYMENT_CREATION_REVIEW", map[string]string{"reason": "expired without gateway trade reference; no new payment submitted"})
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			return err
		}
	}
	return nil
}
