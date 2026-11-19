package service

import (
	"context"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

// Shared by untagged, unit and integration payment test fixtures.
func installPaymentProofTestSchema(t *testing.T, client *dbent.Client) {
	t.Helper()
	if paymentAuditDialect(client) == dialect.Postgres {
		b, err := os.ReadFile("../../migrations/243_payment_verified_receipts.sql")
		require.NoError(t, err)
		_, err = client.ExecContext(context.Background(), string(b))
		require.NoError(t, err)
		_, err = client.ExecContext(context.Background(), "CREATE UNIQUE INDEX IF NOT EXISTS idx_payment_audit_order_action ON payment_audit_logs(order_id,action)")
		require.NoError(t, err)
		return
	}
	_, err := client.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS payment_provider_versions(id INTEGER PRIMARY KEY AUTOINCREMENT,instance_id INTEGER NOT NULL,provider_key TEXT NOT NULL,fingerprint TEXT NOT NULL,namespace TEXT NOT NULL,config TEXT NOT NULL,created_at TEXT DEFAULT CURRENT_TIMESTAMP,UNIQUE(instance_id,fingerprint));
CREATE TABLE IF NOT EXISTS payment_receipts(id INTEGER PRIMARY KEY AUTOINCREMENT,order_id INTEGER NOT NULL UNIQUE REFERENCES payment_orders(id),namespace TEXT NOT NULL,trade_id TEXT NOT NULL,provider_key TEXT NOT NULL,payment_type TEXT NOT NULL,currency TEXT NOT NULL,amount TEXT NOT NULL,chain_identity TEXT UNIQUE,evidence TEXT NOT NULL,verified_at TEXT DEFAULT CURRENT_TIMESTAMP,UNIQUE(namespace,trade_id));
CREATE UNIQUE INDEX IF NOT EXISTS idx_payment_audit_order_action ON payment_audit_logs(order_id,action);`)
	require.NoError(t, err)
}
