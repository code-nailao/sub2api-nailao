package provider

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/shopspring/decimal"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var bepIdentifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var bepDecimal = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)

// BEPusdt implements fixed-chain native payments, not the EasyPay facade.
type BEPusdt struct {
	base                        *url.URL
	token, notifyURL, returnURL string
	client                      *http.Client
	walletAddresses             map[string]string
	rateSource                  string
}

func NewBEPusdt(_ string, cfg map[string]string) (*BEPusdt, error) {
	base, e := url.Parse(strings.TrimRight(strings.TrimSpace(cfg["apiBase"]), "/"))
	if e != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Path != "" {
		return nil, fmt.Errorf("bepusdt apiBase must be an origin URL")
	}
	localHTTP := base.Scheme == "http" && (base.Hostname() == "127.0.0.1" || base.Hostname() == "localhost" || base.Hostname() == "::1")
	if base.Scheme != "https" && !localHTTP {
		return nil, fmt.Errorf("bepusdt requires HTTPS")
	}
	for _, k := range []string{"apiToken", "notifyUrl", "returnUrl"} {
		if strings.TrimSpace(cfg[k]) == "" {
			return nil, fmt.Errorf("bepusdt config missing %s", k)
		}
	}
	for _, k := range []string{"notifyUrl", "returnUrl"} {
		u, e := url.Parse(cfg[k])
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
			return nil, fmt.Errorf("bepusdt %s must be HTTPS", k)
		}
		if utf8.RuneCountInString(cfg[k]) > 255 {
			return nil, fmt.Errorf("bepusdt %s exceeds the gateway's 255-character limit", k)
		}
	}
	walletAddresses, e := payment.ParseBEPusdtWalletAddresses(cfg["walletAddresses"])
	if e != nil {
		return nil, e
	}
	rateSource, e := payment.ParseBEPusdtRateSource(cfg["rateSource"])
	if e != nil {
		return nil, e
	}
	return &BEPusdt{walletAddresses: walletAddresses, rateSource: rateSource, base: base, token: cfg["apiToken"], notifyURL: cfg["notifyUrl"], returnURL: cfg["returnUrl"], client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (*BEPusdt) Name() string        { return "BEpusdt" }
func (*BEPusdt) ProviderKey() string { return payment.TypeBEPusdt }
func (*BEPusdt) SupportedTypes() []payment.PaymentType {
	types := []payment.PaymentType{}
	for _, n := range payment.BEPusdtNetworks() {
		types = append(types, n.Method)
	}
	return types
}
func (*BEPusdt) Refund(context.Context, payment.RefundRequest) (*payment.RefundResponse, error) {
	return nil, payment.ErrRefundUnsupported
}

// The upstream signer decodes JSON numbers to float64 but retains string
// values verbatim. Floating point is used only for signature compatibility.
func bepSign(params map[string]any, token string) (string, error) {
	keys := []string{}
	values := map[string]string{}
	for k, v := range params {
		if k == "signature" || v == nil {
			continue
		}
		var s string
		switch x := v.(type) {
		case string:
			s = x
		case json.Number:
			n, e := strconv.ParseFloat(string(x), 64)
			if e != nil {
				return "", fmt.Errorf("invalid signature number")
			}
			s = fmt.Sprint(n)
		case int:
			s = strconv.Itoa(x)
		case int64:
			s = strconv.FormatInt(x, 10)
		default:
			return "", fmt.Errorf("invalid signature field")
		}
		if s == "" {
			continue
		}
		keys = append(keys, k)
		values[k] = s
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		parts = append(parts, k+"="+values[k])
	}
	sum := md5.Sum([]byte(strings.Join(parts, "&") + token))
	return hex.EncodeToString(sum[:]), nil
}
func (b *BEPusdt) post(ctx context.Context, path string, params map[string]any, signed bool) (json.RawMessage, error) {
	if signed {
		sig, e := bepSign(params, b.token)
		if e != nil {
			return nil, e
		}
		params["signature"] = sig
	}
	body, e := json.Marshal(params)
	if e != nil {
		return nil, fmt.Errorf("bepusdt request encoding failed")
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, b.base.String()+path, bytes.NewReader(body))
	if e != nil {
		return nil, fmt.Errorf("bepusdt request construction failed")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, e := b.client.Do(req)
	if e != nil {
		return nil, fmt.Errorf("bepusdt request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, e := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if e != nil || len(raw) > 1<<20 {
		return nil, fmt.Errorf("bepusdt response invalid")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("bepusdt HTTP status %d", resp.StatusCode)
	}
	var envelope struct {
		StatusCode int             `json:"status_code"`
		Data       json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, fmt.Errorf("bepusdt returned invalid JSON")
	}
	if envelope.StatusCode != 200 {
		// Do not log the remote message: database errors can echo callback
		// credentials or other request fields. The numeric code is sufficient
		// to distinguish a rejected request from a transport outage.
		return nil, fmt.Errorf("bepusdt rejected request (status_code=%d)", envelope.StatusCode)
	}
	if len(envelope.Data) == 0 {
		return nil, fmt.Errorf("bepusdt returned empty data")
	}
	return envelope.Data, nil
}

type bepOrder struct {
	TradeID      string          `json:"trade_id"`
	OrderID      string          `json:"order_id"`
	TradeType    string          `json:"trade_type"`
	Status       int             `json:"status"`
	Amount       json.RawMessage `json:"amount"`
	Money        json.RawMessage `json:"money"`
	ActualAmount string          `json:"actual_amount"`
	Address      string          `json:"token"`
	Fiat         string          `json:"fiat"`
	Expiration   int64           `json:"expiration_time"`
	ExpiresAt    int64           `json:"expired_at"`
	PaymentURL   string          `json:"payment_url"`
	TradeURL     string          `json:"trade_url"`
}

func bepFiat(raw json.RawMessage) (string, error) {
	s := string(raw)
	if len(raw) > 0 && raw[0] == '"' {
		if json.Unmarshal(raw, &s) != nil {
			return "", fmt.Errorf("invalid fiat amount")
		}
	}
	if len(s) > 24 || !bepDecimal.MatchString(s) {
		return "", fmt.Errorf("invalid fiat amount")
	}
	n, e := decimal.NewFromString(s)
	if e != nil || !n.IsPositive() || !n.Equal(n.Round(2)) {
		return "", fmt.Errorf("invalid fiat amount")
	}
	return n.StringFixed(2), nil
}
func bepChainAmount(s string) bool {
	if len(s) > 64 || !bepDecimal.MatchString(s) {
		return false
	}
	n, e := decimal.NewFromString(s)
	return e == nil && n.IsPositive() && n.Exponent() >= -18
}
func bepState(n int) (string, error) {
	switch n {
	case 1:
		return payment.ProviderStatusPending, nil
	case 2:
		return payment.ProviderStatusPaid, nil
	case 3:
		return "expired", nil
	case 4:
		return "cancelled", nil
	case 5:
		return payment.ProviderStatusConfirming, nil
	case 6:
		return "failed", nil
	default:
		return "", fmt.Errorf("unknown BEpusdt state")
	}
}
func (o bepOrder) details() (*payment.CryptoPaymentDetails, error) {
	if !bepIdentifier.MatchString(o.TradeID) || !strings.HasPrefix(o.OrderID, payment.BEPusdtOrderPrefix) || !bepIdentifier.MatchString(o.OrderID) || o.Fiat != "CNY" || !bepChainAmount(o.ActualAmount) {
		return nil, fmt.Errorf("invalid BEpusdt order identity or amounts")
	}
	n, ok := payment.BEPusdtNetworkByTradeType(o.TradeType)
	if !ok {
		return nil, fmt.Errorf("unsupported BEpusdt network")
	}
	address, ok := n.CanonicalAddress(o.Address)
	if !ok {
		return nil, fmt.Errorf("invalid BEpusdt receiving address")
	}
	raw := o.Money
	if len(raw) == 0 {
		raw = o.Amount
	}
	fiat, e := bepFiat(raw)
	if e != nil {
		return nil, e
	}
	state, e := bepState(o.Status)
	if e != nil {
		return nil, e
	}
	hash := ""
	if o.TradeURL != "" {
		u, e := url.Parse(o.TradeURL)
		if e == nil {
			path := u.Path
			if n.Network == "tron" && strings.HasPrefix(u.Fragment, "/transaction/") {
				path = u.Fragment
			}
			parts := strings.Split(strings.Trim(path, "/"), "/")
			candidate := parts[len(parts)-1]
			if canonical, ok := n.CanonicalHash(candidate); ok {
				hash = canonical
			}
		}
	}
	return &payment.CryptoPaymentDetails{Asset: "USDT", Network: n.Network, TradeType: o.TradeType, TradeID: o.TradeID, Fiat: "CNY", FiatAmount: fiat, ChainAmount: o.ActualAmount, Address: address, Status: state, TransactionHash: hash, ExpiresAt: time.Unix(o.ExpiresAt, 0)}, nil
}
func (b *BEPusdt) CreatePayment(ctx context.Context, req payment.CreatePaymentRequest) (*payment.CreatePaymentResponse, error) {
	tradeType, _, ok := payment.BEPusdtTradeType(req.PaymentType)
	if !ok || !bepIdentifier.MatchString(req.OrderID) || !strings.HasPrefix(req.OrderID, payment.BEPusdtOrderPrefix) {
		return nil, fmt.Errorf("invalid BEpusdt method or order namespace")
	}
	amount, e := bepFiat(json.RawMessage(strconv.Quote(req.Amount)))
	if e != nil {
		return nil, e
	}
	if utf8.RuneCountInString(req.Subject) > 64 {
		return nil, fmt.Errorf("bepusdt product name exceeds the gateway's 64-character limit")
	}
	timeout := int64(time.Until(req.ExpiresAt).Seconds())
	if timeout < 180 || timeout > 3600 {
		return nil, fmt.Errorf("bepusdt deadline must be within 180..3600 seconds")
	}
	returnURL := req.ReturnURL
	if returnURL == "" {
		returnURL = b.returnURL
	}
	returnURL, e = bepCheckoutReturnURL(returnURL)
	if e != nil {
		return nil, e
	}
	rate := ""
	if b.rateSource == payment.BEPusdtRateBinance {
		rate, e = b.binanceReferenceRate(ctx)
		if e != nil {
			// No gateway creation has been attempted. Do not classify this as
			// an unknown payment outcome or silently use the shared gateway rate.
			return nil, e
		}
		timeout = int64(time.Until(req.ExpiresAt).Seconds())
		if timeout < 180 {
			return nil, fmt.Errorf("bepusdt deadline elapsed while obtaining reference rate")
		}
	}
	params := map[string]any{"order_id": req.OrderID, "amount": json.Number(amount), "notify_url": b.notifyURL, "redirect_url": returnURL, "trade_type": tradeType, "fiat": "CNY", "timeout": timeout, "name": req.Subject}
	if rate != "" {
		params["rate"] = rate
	}
	if address := b.walletAddresses[req.PaymentType]; address != "" {
		params["address"] = address
	}
	raw, e := b.post(ctx, "/api/v1/order/create-transaction", params, true)
	if e != nil {
		return nil, fmt.Errorf("%w: BEpusdt creation needs reconciliation: %v", payment.ErrPaymentOutcomeUnknown, e)
	}
	var o bepOrder
	if json.Unmarshal(raw, &o) != nil {
		return nil, payment.ErrPaymentOutcomeUnknown
	}
	q, e := o.details()
	if e != nil {
		return nil, payment.ErrPaymentOutcomeUnknown
	}
	if o.OrderID != req.OrderID || o.TradeType != tradeType || q.FiatAmount != amount || !bepDeadlineMatches(req.ExpiresAt, o.Expiration, time.Now()) || q.Status != payment.ProviderStatusPending {
		return nil, payment.ErrPaymentOutcomeUnknown
	}
	if address := b.walletAddresses[req.PaymentType]; address != "" && q.Address != address {
		return nil, payment.ErrPaymentOutcomeUnknown
	}
	payURL, e := url.Parse(o.PaymentURL)
	if e != nil {
		return nil, payment.ErrPaymentOutcomeUnknown
	}
	payURL = b.base.ResolveReference(payURL)
	if payURL.Scheme != b.base.Scheme || payURL.Host != b.base.Host || payURL.User != nil || payURL.Fragment != "" || payURL.RawQuery != "" || payURL.Path != "/pay/checkout/"+o.TradeID {
		return nil, payment.ErrPaymentOutcomeUnknown
	}
	q.ReferenceRate, q.RateSource = rate, b.rateSource
	q.ExpiresAt = time.Now().Add(time.Duration(o.Expiration) * time.Second)
	if q.ExpiresAt.After(req.ExpiresAt) {
		q.ExpiresAt = req.ExpiresAt
	}
	return &payment.CreatePaymentResponse{TradeNo: o.TradeID, PayURL: payURL.String(), Currency: "CNY", CryptoPayment: q}, nil
}

// expiration_time is remaining time at response generation, not the original
// requested timeout. Compare deadlines so gateway processing time is not mistaken
// for a silently shortened/default TTL.
func bepDeadlineMatches(deadline time.Time, remainingSeconds int64, receivedAt time.Time) bool {
	if remainingSeconds <= 0 || remainingSeconds > 3600 {
		return false
	}
	reported := receivedAt.Add(time.Duration(remainingSeconds) * time.Second)
	return !reported.Before(deadline.Add(-5*time.Second)) && !reported.After(deadline.Add(5*time.Second))
}

// The hosted gateway stores return_url in varchar(255). Main's signed resume
// credential remains in its authenticated create response/recovery snapshot;
// the gateway only needs public order references to navigate back. The return
// page verifies the order server-side and never trusts URL payment-status claims.
func bepCheckoutReturnURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", fmt.Errorf("invalid BEpusdt return URL")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", fmt.Errorf("invalid BEpusdt return URL query")
	}
	query.Del("resume_token")
	u.RawQuery = query.Encode()
	result := u.String()
	if utf8.RuneCountInString(result) > 255 {
		return "", fmt.Errorf("bepusdt return URL exceeds the gateway's 255-character limit")
	}
	return result, nil
}
func (b *BEPusdt) QueryOrder(ctx context.Context, tradeID string) (*payment.QueryOrderResponse, error) {
	if !bepIdentifier.MatchString(tradeID) {
		return nil, fmt.Errorf("invalid BEpusdt trade ID")
	}
	raw, e := b.post(ctx, "/api/v1/pay/info", map[string]any{"trade_id": tradeID}, false)
	if e != nil {
		return nil, e
	}
	var o bepOrder
	if json.Unmarshal(raw, &o) != nil || o.TradeID != tradeID {
		return nil, fmt.Errorf("%w: query identity", payment.ErrInvalidPaymentEvidence)
	}
	q, e := o.details()
	if e != nil {
		return nil, fmt.Errorf("%w: %v", payment.ErrInvalidPaymentEvidence, e)
	}
	if o.ExpiresAt <= 0 {
		return nil, fmt.Errorf("%w: missing expiry", payment.ErrInvalidPaymentEvidence)
	}
	n, _ := decimal.NewFromString(q.FiatAmount)
	amount, _ := n.Float64()
	return &payment.QueryOrderResponse{OrderID: o.OrderID, Currency: "CNY", AmountExact: q.FiatAmount, PayURL: b.base.String() + "/pay/checkout/" + o.TradeID, TradeNo: o.TradeID, Status: q.Status, Amount: amount, CryptoPayment: q, Metadata: map[string]string{"order_id": o.OrderID, "currency": "CNY", "trade_type": o.TradeType, "address": q.Address, "chain_amount": o.ActualAmount, "fiat_amount": q.FiatAmount, "transaction_hash": q.TransactionHash}}, nil
}
func (b *BEPusdt) VerifyNotification(_ context.Context, raw string, _ map[string]string) (*payment.PaymentNotification, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var params map[string]any
	if dec.Decode(&params) != nil {
		return nil, fmt.Errorf("invalid BEpusdt notification")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("invalid notification tail")
	}
	signature, ok := params["signature"].(string)
	if !ok {
		return nil, fmt.Errorf("missing BEpusdt signature")
	}
	expected, e := bepSign(params, b.token)
	if e != nil {
		return nil, e
	}
	actualBytes, e := hex.DecodeString(signature)
	expectedBytes, _ := hex.DecodeString(expected)
	if e != nil || !hmac.Equal(actualBytes, expectedBytes) {
		return nil, fmt.Errorf("invalid BEpusdt signature")
	}
	state, ok := params["status"].(json.Number)
	if !ok {
		return nil, fmt.Errorf("invalid BEpusdt state")
	}
	stateInt, e := state.Int64()
	if e != nil {
		return nil, fmt.Errorf("invalid BEpusdt state")
	}
	if _, e = bepState(int(stateInt)); e != nil {
		return nil, e
	}
	if stateInt != 2 {
		return nil, nil
	}
	orderID, _ := params["order_id"].(string)
	tradeID, _ := params["trade_id"].(string)
	address, _ := params["token"].(string)
	chainAmount, _ := params["actual_amount"].(string)
	hash, _ := params["block_transaction_id"].(string)
	if !bepIdentifier.MatchString(tradeID) || !strings.HasPrefix(orderID, payment.BEPusdtOrderPrefix) || !bepIdentifier.MatchString(orderID) || !bepChainAmount(chainAmount) {
		return nil, fmt.Errorf("invalid BEpusdt paid notification")
	}
	validIdentity := false
	for _, network := range payment.BEPusdtNetworks() {
		_, a := network.CanonicalAddress(address)
		_, h := network.CanonicalHash(hash)
		if a && h {
			validIdentity = true
			break
		}
	}
	if !validIdentity {
		return nil, fmt.Errorf("invalid BEpusdt address or transaction hash")
	}
	moneyRaw, _ := json.Marshal(params["amount"])
	fiat, e := bepFiat(moneyRaw)
	if e != nil {
		return nil, e
	}
	// Verify signed claims here; the service binds them to one authoritative query.
	value, _ := decimal.NewFromString(fiat)
	amount, _ := value.Float64()
	return &payment.PaymentNotification{OrderID: orderID, TradeNo: tradeID, Amount: amount, Status: payment.NotificationStatusSuccess, Metadata: map[string]string{"order_id": orderID, "fiat_amount": fiat, "chain_amount": chainAmount, "address": address, "transaction_hash": hash}}, nil
}
func (b *BEPusdt) CancelPayment(ctx context.Context, tradeID string) error {
	if !bepIdentifier.MatchString(tradeID) {
		return fmt.Errorf("invalid BEpusdt trade ID")
	}
	raw, e := b.post(ctx, "/api/v1/order/cancel-transaction", map[string]any{"trade_id": tradeID}, true)
	if e != nil {
		return e
	}
	var result struct {
		TradeID string `json:"trade_id"`
	}
	if json.Unmarshal(raw, &result) != nil || result.TradeID != tradeID {
		return fmt.Errorf("bepusdt cancellation unconfirmed")
	}
	return nil
}
