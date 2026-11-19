-- Additive, backend-only credential versions and verified payment receipts.
-- Do not backfill a receipt from local COMPLETED status: it is not payment proof.
CREATE TABLE IF NOT EXISTS payment_provider_versions (
 id BIGSERIAL PRIMARY KEY,
 instance_id BIGINT NOT NULL,
 provider_key VARCHAR(30) NOT NULL,
 fingerprint VARCHAR(64) NOT NULL,
 namespace VARCHAR(64) NOT NULL,
 config TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS paymentproviderversion_instance_id_fingerprint
 ON payment_provider_versions(instance_id, fingerprint);
CREATE TABLE IF NOT EXISTS payment_receipts (
 id BIGSERIAL PRIMARY KEY,
 order_id BIGINT NOT NULL UNIQUE REFERENCES payment_orders(id) ON DELETE RESTRICT,
 namespace VARCHAR(64) NOT NULL,
 trade_id VARCHAR(128) NOT NULL,
 provider_key VARCHAR(30) NOT NULL,
 payment_type VARCHAR(30) NOT NULL,
 currency VARCHAR(10) NOT NULL,
 amount VARCHAR(80) NOT NULL,
 chain_identity VARCHAR(64) UNIQUE,
 evidence JSONB NOT NULL,
 verified_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS paymentreceipt_namespace_trade_id
 ON payment_receipts(namespace, trade_id);

-- Preserve the historical built-in defaults explicitly, never arbitrary methods.
UPDATE payment_provider_instances SET supported_types = CASE provider_key
 WHEN 'easypay' THEN 'alipay,wxpay' WHEN 'alipay' THEN 'alipay'
 WHEN 'wxpay' THEN 'wxpay' WHEN 'stripe' THEN 'card,link'
 WHEN 'airwallex' THEN 'airwallex' ELSE supported_types END
WHERE supported_types = '';
