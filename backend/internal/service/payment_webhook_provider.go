package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentorder"
	"github.com/Wei-Shaw/sub2api/ent/paymentproviderinstance"
	"github.com/Wei-Shaw/sub2api/internal/payment"
)

// GetWebhookProvider returns the provider instance that should verify a webhook.
// It resolves the original provider instance from the order whenever possible and
// resolves current database providers for legacy/single-instance scenarios.
func (s *PaymentService) GetWebhookProvider(ctx context.Context, providerKey, outTradeNo string) (payment.Provider, error) {
	providers, err := s.GetWebhookProviders(ctx, providerKey, outTradeNo)
	if err != nil {
		return nil, err
	}
	if len(providers) == 0 {
		return nil, payment.ErrProviderNotFound
	}
	return providers[0], nil
}

// GetWebhookProviders returns provider candidates that can verify the webhook.
// Official WeChat Pay may require multiple candidates because the callback body
// cannot be bound to a merchant before decryption.
func (s *PaymentService) GetWebhookProviders(ctx context.Context, providerKey, outTradeNo string) ([]payment.Provider, error) {
	if outTradeNo != "" {
		order, err := s.entClient.PaymentOrder.Query().Where(paymentorder.OutTradeNo(outTradeNo)).Only(ctx)
		if oid, legacy := parseLegacyPaymentOrderID(outTradeNo, err); legacy {
			order, err = s.entClient.PaymentOrder.Get(ctx, oid)
			if err != nil || order.OutTradeNo != "" {
				return nil, ErrOrderNotFound
			}
		}
		if err != nil {
			if !dbent.IsNotFound(err) {
				return nil, fmt.Errorf("webhook order lookup failed: %w", err)
			}
			if verifiedPaymentProvider(providerKey) {
				return nil, ErrOrderNotFound
			}
		}
		if err == nil {
			if psHasPinnedProviderInstance(order) {
				prov, err := s.getPinnedOrderProvider(ctx, order)
				if err != nil {
					return nil, err
				}
				return []payment.Provider{prov}, nil
			}
			inst, err := s.getOrderProviderInstance(ctx, order)
			if err != nil {
				return nil, fmt.Errorf("load order provider instance: %w", err)
			}
			if inst != nil {
				prov, err := s.createProviderFromInstance(ctx, inst)
				if err != nil {
					return nil, err
				}
				return []payment.Provider{prov}, nil
			}
			return s.getEnabledWebhookProvidersByKey(ctx, providerKey)
		}
	}

	return s.getEnabledWebhookProvidersByKey(ctx, providerKey)
}

func (s *PaymentService) getPinnedOrderProvider(ctx context.Context, o *dbent.PaymentOrder) (payment.Provider, error) {
	if p, known, err := s.resolveVerifiedProvider(ctx, o); known {
		return p, err
	}
	inst, err := s.getOrderProviderInstance(ctx, o)
	if err != nil {
		return nil, fmt.Errorf("load order provider instance: %w", err)
	}
	if inst == nil {
		return nil, fmt.Errorf("order %d provider instance is missing", o.ID)
	}
	return s.createProviderFromInstance(ctx, inst)
}

func psHasPinnedProviderInstance(order *dbent.PaymentOrder) bool {
	return order != nil && (psOrderProviderSnapshot(order) != nil || (order.ProviderInstanceID != nil && strings.TrimSpace(*order.ProviderInstanceID) != ""))
}

// No process-local registry is consulted: admin and webhook requests may be
// served by different processes. Check ambiguity before constructing providers
// so a bad configuration cannot hide a second merchant.
func (s *PaymentService) getEnabledWebhookProvidersByKey(ctx context.Context, providerKey string) ([]payment.Provider, error) {
	providerKey = strings.TrimSpace(providerKey)
	instances, err := s.entClient.PaymentProviderInstance.Query().
		Where(
			paymentproviderinstance.ProviderKeyEQ(providerKey),
			paymentproviderinstance.EnabledEQ(true),
		).
		Order(dbent.Asc(paymentproviderinstance.FieldSortOrder)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query webhook provider instances: %w", err)
	}
	if len(instances) == 0 {
		return nil, payment.ErrProviderNotFound
	}
	if providerKey != payment.TypeWxpay && len(instances) != 1 {
		return nil, fmt.Errorf("webhook provider fallback is ambiguous for %s", providerKey)
	}

	providers := make([]payment.Provider, 0, len(instances))
	for _, inst := range instances {
		prov, provErr := s.createProviderFromInstance(ctx, inst)
		if provErr != nil {
			slog.Warn("skip webhook provider instance", "provider", providerKey, "instanceID", inst.ID, "error", provErr)
			continue
		}
		providers = append(providers, prov)
	}
	if len(providers) == 0 {
		return nil, payment.ErrProviderNotFound
	}
	return providers, nil
}
