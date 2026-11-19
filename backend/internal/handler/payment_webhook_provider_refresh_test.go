//go:build unit

package handler

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	stripe "github.com/stripe/stripe-go/v85"
	"github.com/stripe/stripe-go/v85/webhook"
	_ "modernc.org/sqlite"
)

func newWebhookRefreshClient(t *testing.T) (*dbent.Client, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(entsql.OpenDB(dialect.SQLite, db))))
	t.Cleanup(func() { _ = client.Close() })
	return client, db
}

func newWebhookRefreshHandler(client *dbent.Client) *PaymentWebhookHandler {
	registry := payment.NewRegistry()
	svc := service.NewPaymentService(client, registry, payment.NewDefaultLoadBalancer(client, nil), nil, nil, nil, nil, nil, nil)
	return NewPaymentWebhookHandler(svc, registry)
}

func TestPaymentWebhookProviderLookupFailureIsRetryable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client, db := newWebhookRefreshClient(t)
	h := newWebhookRefreshHandler(client)
	require.NoError(t, db.Close())
	for _, key := range []string{payment.TypeStripe, payment.TypeAirwallex, payment.TypeAlipay, payment.TypeWxpay, payment.TypeBEPusdt, payment.TypeEasyPay} {
		t.Run(key, func(t *testing.T) {
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/payment/webhook/"+key, strings.NewReader("{}"))
			h.handleNotify(c, key)
			require.Equal(t, http.StatusServiceUnavailable, response.Code)
			require.NotContains(t, response.Body.String(), "database")
		})
	}
}

func TestPaymentWebhookCurrentSignatureStillAcknowledgesVerifiedUnknownOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client, _ := newWebhookRefreshClient(t)
	ctx := context.Background()
	inst, err := client.PaymentProviderInstance.Create().SetProviderKey(payment.TypeStripe).
		SetName("isolated stripe").SetSupportedTypes(payment.TypeStripe).SetEnabled(true).
		SetConfig(`{"secretKey":"sk_test_isolated","webhookSecret":"whsec_old_test"}`).Save(ctx)
	require.NoError(t, err)
	h := newWebhookRefreshHandler(client)
	h.paymentService.EnsureProviders(ctx)
	_, err = client.PaymentProviderInstance.UpdateOneID(inst.ID).
		SetConfig(`{"secretKey":"sk_test_isolated","webhookSecret":"whsec_current_test"}`).Save(ctx)
	require.NoError(t, err)
	for _, secret := range []string{"whsec_old_test", "whsec_current_test"} {
		t.Run(secret, func(t *testing.T) {
			body := fmt.Sprintf(`{"id":"evt_missing_order","object":"event","api_version":%q,"type":"payment_intent.succeeded","data":{"object":{"id":"pi_missing_order","object":"payment_intent","amount":8800,"currency":"cny","metadata":{"orderId":"missing-order"}}}}`, stripe.APIVersion)
			signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: []byte(body), Secret: secret})
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/payment/webhook/stripe", strings.NewReader(body))
			c.Request.Header.Set("Stripe-Signature", signed.Header)
			h.StripeWebhook(c)
			if secret == "whsec_old_test" {
				require.Equal(t, http.StatusBadRequest, response.Code)
			} else {
				require.Equal(t, http.StatusOK, response.Code)
				require.Empty(t, response.Body.String())
			}
		})
	}
}
