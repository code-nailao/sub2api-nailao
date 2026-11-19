// Package proof owns the existing verified-payment evidence tables. It does not
// own user balances, subscriptions, payment UI, workers, or provider requests.
package proof

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// DB is also implemented by Ent's transaction client, so evidence and the
// caller's order/audit updates commit or roll back together.
type DB interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

var ErrConflict = errors.New("payment evidence conflicts with an existing order")

func Digest(parts ...string) string {
	b, _ := json.Marshal(parts)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Namespace intentionally excludes mutable instance IDs and credentials.
// This framing remains byte-compatible with existing Main payment receipts.
func Namespace(key string, cfg map[string]string) string {
	merchant := cfg["pid"]
	switch key {
	case "bepusdt":
		merchant = "gateway"
	case "alipay":
		merchant = cfg["appId"]
	case "wxpay":
		merchant = cfg["mchId"]
	case "airwallex":
		merchant = cfg["accountId"]
	case "stripe":
		merchant = "stripe"
	}
	base := strings.TrimRight(strings.TrimSpace(cfg["apiBase"]), "/")
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		u.Host = strings.ToLower(u.Host)
		if u.Scheme == "https" && u.Port() == "443" {
			u.Host = u.Hostname()
		}
		u.RawQuery, u.Fragment, u.RawPath = "", "", ""
		if key == "easypay" {
			for _, suffix := range []string{"/submit.php", "/mapi.php", "/api.php"} {
				if strings.HasSuffix(strings.ToLower(u.Path), suffix) {
					u.Path = u.Path[:len(u.Path)-len(suffix)]
				}
			}
		}
		if key == "airwallex" && (u.Path == "" || u.Path == "/") {
			u.Path = "/api/v1"
		}
		base = strings.TrimRight(u.String(), "/")
	}
	return Digest(key, base, merchant)
}

func Pin(ctx context.Context, db DB, instance int64, key string, cfg map[string]string) (int64, string, error) {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return 0, "", err
	}
	ns, fingerprint := Namespace(key, cfg), Digest(key, string(encoded))
	_, err = db.ExecContext(ctx, `INSERT INTO payment_provider_versions(instance_id,provider_key,fingerprint,namespace,config)
VALUES($1,$2,$3,$4,$5) ON CONFLICT(instance_id,fingerprint) DO NOTHING`, instance, key, fingerprint, ns, string(encoded))
	if err != nil {
		return 0, "", err
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM payment_provider_versions WHERE instance_id=$1 AND fingerprint=$2`, instance, fingerprint)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return 0, "", errors.Join(sql.ErrNoRows, rows.Err())
	}
	var id int64
	err = rows.Scan(&id)
	return id, ns, err
}

func Resolve(ctx context.Context, db DB, id, instance int64, key, namespace string) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT instance_id,provider_key,fingerprint,namespace,config FROM payment_provider_versions WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, errors.Join(sql.ErrNoRows, rows.Err())
	}
	var actualInstance int64
	var actualKey, fingerprint, ns, raw string
	if err = rows.Scan(&actualInstance, &actualKey, &fingerprint, &ns, &raw); err != nil {
		return nil, err
	}
	var cfg map[string]string
	if json.Unmarshal([]byte(raw), &cfg) != nil || actualInstance != instance || key != actualKey || ns != namespace || fingerprint != Digest(key, raw) || ns != Namespace(key, cfg) {
		return nil, ErrConflict
	}
	return cfg, nil
}

type Receipt struct {
	OrderID                                                                       int64
	Namespace, TradeID, ProviderKey, PaymentType, Currency, Amount, ChainIdentity string
	Evidence                                                                      map[string]string
}

func Load(ctx context.Context, db DB, orderID int64) (*Receipt, error) {
	rows, err := db.QueryContext(ctx, `SELECT order_id,namespace,trade_id,provider_key,payment_type,currency,amount,COALESCE(chain_identity,''),evidence FROM payment_receipts WHERE order_id=$1`, orderID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	r := &Receipt{}
	var raw []byte
	if err := rows.Scan(&r.OrderID, &r.Namespace, &r.TradeID, &r.ProviderKey, &r.PaymentType, &r.Currency, &r.Amount, &r.ChainIdentity, &raw); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &r.Evidence); err != nil {
		return nil, err
	}
	return r, nil
}

func Save(ctx context.Context, db DB, r *Receipt) error {
	if r == nil || r.OrderID <= 0 || r.Namespace == "" || r.TradeID == "" {
		return ErrConflict
	}
	old, err := Load(ctx, db, r.OrderID)
	if err == nil {
		left, _ := json.Marshal(old)
		right, _ := json.Marshal(r)
		if string(left) != string(right) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	data, err := json.Marshal(r.Evidence)
	if err != nil {
		return err
	}
	var chain any
	if r.ChainIdentity != "" {
		chain = r.ChainIdentity
	}
	_, err = db.ExecContext(ctx, `INSERT INTO payment_receipts(order_id,namespace,trade_id,provider_key,payment_type,currency,amount,chain_identity,evidence)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, r.OrderID, r.Namespace, r.TradeID, r.ProviderKey, r.PaymentType, r.Currency, r.Amount, chain, string(data))
	if err != nil {
		return fmt.Errorf("persist payment evidence: %w", err)
	}
	return nil
}
