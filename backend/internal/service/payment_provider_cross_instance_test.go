//go:build unit

package service

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strconv"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/stretchr/testify/require"
	stripe "github.com/stripe/stripe-go/v85"
	"github.com/stripe/stripe-go/v85/webhook"
)

func newCrossInstancePaymentService(client *dbent.Client) *PaymentService {
	return &PaymentService{
		entClient:    client,
		registry:     payment.NewRegistry(),
		loadBalancer: newWebhookProviderTestLoadBalancer(client),
	}
}

func createCrossInstanceStripe(t *testing.T, client *dbent.Client, name, secret string) *dbent.PaymentProviderInstance {
	t.Helper()
	inst, err := client.PaymentProviderInstance.Create().
		SetProviderKey(payment.TypeStripe).SetName(name).
		SetConfig(encryptWebhookProviderConfig(t, map[string]string{
			"secretKey": "sk_test_" + name, "webhookSecret": secret,
		})).SetSupportedTypes(payment.TypeStripe).SetEnabled(true).Save(context.Background())
	require.NoError(t, err)
	return inst
}

func signedCrossInstanceStripe(secret string) (string, map[string]string) {
	body := fmt.Sprintf(`{"id":"evt_isolated","object":"event","api_version":%q,"type":"payment_intent.succeeded","data":{"object":{"id":"pi_isolated","object":"payment_intent","amount":8800,"currency":"cny","metadata":{"orderId":"sub2_isolated"}}}}`, stripe.APIVersion)
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: []byte(body), Secret: secret})
	return body, map[string]string{"stripe-signature": signed.Header}
}

func TestWebhookProviderObservesAnotherInstanceCredentialChangeImmediately(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	inst := createCrossInstanceStripe(t, client, "old", "whsec_old_test")
	serving := newCrossInstancePaymentService(client)
	serving.EnsureProviders(ctx)
	oldProvider, err := serving.registry.GetProviderByKey(payment.TypeStripe)
	require.NoError(t, err)
	oldBody, oldHeaders := signedCrossInstanceStripe("whsec_old_test")
	_, err = oldProvider.VerifyNotification(ctx, oldBody, oldHeaders)
	require.NoError(t, err)

	// The admin instance persists the update and refreshes only its own registry.
	admin := NewPaymentConfigService(client, nil, []byte(webhookProviderTestEncryptionKey))
	_, err = admin.UpdateProviderInstance(ctx, inst.ID, UpdateProviderInstanceRequest{
		Config: map[string]string{"secretKey": "sk_test_rotated", "webhookSecret": "whsec_new_test"},
	})
	require.NoError(t, err)
	owner := newCrossInstancePaymentService(client)
	owner.RefreshProviders(ctx)
	newBody, newHeaders := signedCrossInstanceStripe("whsec_new_test")
	_, err = oldProvider.VerifyNotification(ctx, newBody, newHeaders)
	require.Error(t, err, "the serving registry is deliberately stale")

	providers, err := serving.GetWebhookProviders(ctx, payment.TypeStripe, "")
	require.NoError(t, err)
	require.Len(t, providers, 1)
	notification, err := providers[0].VerifyNotification(ctx, newBody, newHeaders)
	require.NoError(t, err)
	require.Equal(t, "sub2_isolated", notification.OrderID)
	require.Equal(t, 88.0, notification.Amount)
	_, err = providers[0].VerifyNotification(ctx, oldBody, oldHeaders)
	require.Error(t, err, "an unbound webhook must no longer accept the old key")
}

func TestWebhookProviderObservesNewMerchantAfterEmptyRegistryWarmup(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	serving := newCrossInstancePaymentService(client)
	serving.EnsureProviders(ctx)
	createCrossInstanceStripe(t, client, "new", "whsec_new_merchant_test")
	providers, err := serving.GetWebhookProviders(ctx, payment.TypeStripe, "")
	require.NoError(t, err)
	require.Len(t, providers, 1)
	body, headers := signedCrossInstanceStripe("whsec_new_merchant_test")
	_, err = providers[0].VerifyNotification(ctx, body, headers)
	require.NoError(t, err)
}

func TestWebhookProviderDoesNotReuseRemovedOrDisabledCachedMerchant(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(fmt.Sprintf("delete_%t", remove), func(t *testing.T) {
			ctx := context.Background()
			client := newPaymentConfigServiceTestClient(t)
			inst := createCrossInstanceStripe(t, client, "retired", "whsec_retired_test")
			serving := newCrossInstancePaymentService(client)
			serving.EnsureProviders(ctx)
			if remove {
				require.NoError(t, client.PaymentProviderInstance.DeleteOneID(inst.ID).Exec(ctx))
			} else {
				_, err := client.PaymentProviderInstance.UpdateOneID(inst.ID).SetEnabled(false).Save(ctx)
				require.NoError(t, err)
			}
			providers, err := serving.GetWebhookProviders(ctx, payment.TypeStripe, "")
			require.ErrorIs(t, err, payment.ErrProviderNotFound)
			require.Nil(t, providers)
		})
	}
}

func TestWebhookProviderFailsClosedOnDatabaseReadFailure(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	createCrossInstanceStripe(t, client, "cached", "whsec_cached_test")
	serving := newCrossInstancePaymentService(client)
	serving.EnsureProviders(ctx)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	providers, err := serving.GetWebhookProviders(cancelled, payment.TypeStripe, "")
	require.Error(t, err)
	require.Nil(t, providers)
}

func TestLegacyOrderFallbackUsesCurrentMerchantAfterAnotherInstanceChange(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	old := createCrossInstanceStripe(t, client, "legacy", "whsec_old_test")
	serving := newCrossInstancePaymentService(client)
	serving.EnsureProviders(ctx)
	_, err := client.PaymentProviderInstance.UpdateOneID(old.ID).SetEnabled(false).Save(ctx)
	require.NoError(t, err)
	createCrossInstanceStripe(t, client, "current", "whsec_current_test")
	order := &dbent.PaymentOrder{ID: 44, PaymentType: payment.TypeStripe}
	current, err := serving.getOrderProvider(ctx, order)
	require.NoError(t, err)
	body, headers := signedCrossInstanceStripe("whsec_current_test")
	_, err = current.VerifyNotification(ctx, body, headers)
	require.NoError(t, err)
	oldBody, oldHeaders := signedCrossInstanceStripe("whsec_old_test")
	_, err = current.VerifyNotification(ctx, oldBody, oldHeaders)
	require.Error(t, err)

	createCrossInstanceStripe(t, client, "ambiguous", "whsec_ambiguous_test")
	_, err = serving.getOrderProvider(ctx, order)
	require.ErrorContains(t, err, "ambiguous")
}

func TestPinnedFourChainWebhookKeepsOriginalKeyAfterAnotherInstanceRotation(t *testing.T) {
	for _, method := range []string{"usdt_bsc", "usdt_arbitrum", "usdt_tron", "usdt_xlayer"} {
		t.Run(method, func(t *testing.T) {
			ctx := context.Background()
			f := newVerifiedFixture(t, method)
			snapshot := buildPaymentOrderProviderSnapshot(f.selection, CreateOrderRequest{})
			require.NoError(t, f.svc.pinVerifiedProvider(ctx, f.svc.entClient, f.selection, snapshot))
			_, err := f.svc.entClient.PaymentOrder.UpdateOneID(f.order.ID).SetProviderSnapshot(snapshot).Save(ctx)
			require.NoError(t, err)
			id, err := strconv.ParseInt(f.selection.InstanceID, 10, 64)
			require.NoError(t, err)
			_, err = f.svc.entClient.PaymentProviderInstance.UpdateOneID(id).SetEnabled(false).
				SetConfig(`{"apiBase":"https://wrong.example","apiToken":"rotated"}`).Save(ctx)
			require.NoError(t, err)

			serving := newCrossInstancePaymentService(f.svc.entClient)
			providers, err := serving.GetWebhookProviders(ctx, payment.TypeBEPusdt, f.order.OutTradeNo)
			require.NoError(t, err)
			require.Len(t, providers, 1)
			for _, token := range []string{"test-only", "rotated"} {
				// A pending signed callback verifies the key without claiming payment.
				sum := md5.Sum([]byte("status=1" + token))
				body := fmt.Sprintf(`{"status":1,"signature":%q}`, hex.EncodeToString(sum[:]))
				_, err := providers[0].VerifyNotification(ctx, body, nil)
				if token == "test-only" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			}
			result, err := providers[0].QueryOrder(ctx, f.order.PaymentTradeNo)
			require.NoError(t, err, "must use the original local gateway, never the rotated endpoint")
			network, _ := payment.BEPusdtNetworkByMethod(method)
			require.Equal(t, network.Network, result.CryptoPayment.Network)
			require.Zero(t, f.credits.Load())
		})
	}
}
