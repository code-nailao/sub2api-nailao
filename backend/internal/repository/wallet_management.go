package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type walletRegistryRepository struct{ db *sql.DB }

func NewWalletRegistryRepository(db *sql.DB) service.WalletRegistryRepository {
	return &walletRegistryRepository{db: db}
}

func decodeWalletRegistry(raw []byte) (service.WalletRegistry, error) {
	var state service.WalletRegistry
	if len(raw) > 8<<20 || json.Unmarshal(raw, &state) != nil || state.Version != 1 {
		return state, errors.New("invalid_wallet_registry")
	}
	state.Initialize()
	return state, nil
}

func (r *walletRegistryRepository) Read(ctx context.Context) (service.WalletRegistry, error) {
	var raw []byte
	err := r.db.QueryRowContext(ctx, `SELECT value FROM sources_wallet_management WHERE id=1`).Scan(&raw)
	if err != nil {
		return service.WalletRegistry{}, err
	}
	return decodeWalletRegistry(raw)
}

func (r *walletRegistryRepository) Change(ctx context.Context, change func(*service.WalletRegistry) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='3s'; SET LOCAL statement_timeout='5s'`); err != nil {
		return err
	}
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT value FROM sources_wallet_management WHERE id=1 FOR UPDATE`).Scan(&raw); err != nil {
		return err
	}
	state, err := decodeWalletRegistry(raw)
	if err != nil {
		return err
	}
	before, err := json.Marshal(state)
	if err != nil {
		return err
	}
	previousMembers := make(map[int64]service.WalletMember, len(state.Members))
	for id, member := range state.Members {
		previousMembers[id] = member
	}
	if err = change(&state); err != nil {
		return err
	}
	after, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(after) > 8<<20 {
		return errors.New("wallet_registry_capacity")
	}
	if !bytes.Equal(before, after) {
		if _, err = tx.ExecContext(ctx, `UPDATE sources_wallet_management SET value=$1::jsonb,updated_at=NOW() WHERE id=1`, string(after)); err != nil {
			return err
		}
		// Only representatives remain due. This is atomic with the membership
		// change, so old non-representative rows cannot starve the due-page scan.
		if _, err = tx.ExecContext(ctx, `UPDATE sources_account_wallet_snapshots s SET next_check_at=NULL,
            value=s.value || jsonb_build_object('auto_refresh',false,'next_check_at',NULL)
            FROM sources_wallet_management m WHERE m.id=1 AND s.next_check_at IS NOT NULL
            AND coalesce(m.value->'members'->(s.account_id::text)->>'wallet_id','')<>''
            AND NOT (coalesce(m.value->'profiles'->(m.value->'members'->(s.account_id::text)->>'wallet_id')->>'provider','')='sub'
            AND coalesce(m.value->'profiles'->(m.value->'members'->(s.account_id::text)->>'wallet_id')->>'representative_id','0')::bigint=s.account_id)`); err != nil {
			return err
		}
		// A prospective representative was verified while its independent
		// schedule was suppressed. Start its shared schedule after the switch.
		if _, err = tx.ExecContext(ctx, `UPDATE sources_account_wallet_snapshots s
            SET next_check_at=NOW()+INTERVAL '5 minutes',
                value=s.value || jsonb_build_object('auto_refresh',true,'next_check_at',NOW()+INTERVAL '5 minutes')
            FROM sources_wallet_management m WHERE m.id=1 AND s.next_check_at IS NULL
            AND (s.lease_until IS NULL OR s.lease_until<=NOW())
            AND s.identity=m.value->'members'->(s.account_id::text)->>'identity'
            AND m.value->'profiles'->(m.value->'members'->(s.account_id::text)->>'wallet_id')->>'provider'='sub'
            AND coalesce(m.value->'profiles'->(m.value->'members'->(s.account_id::text)->>'wallet_id')->>'representative_id','0')::bigint=s.account_id
            AND s.value->>'status'='ready' AND s.value->>'remaining' IS NOT NULL
            AND (s.value->>'checked_at')::timestamptz > NOW()-INTERVAL '12 minutes'`); err != nil {
			return err
		}
		for id, old := range previousMembers {
			if old.WalletID != "" && state.Members[id].WalletID == "" {
				if _, err = tx.ExecContext(ctx, `UPDATE sources_account_wallet_snapshots SET identity='unlinked',next_check_at=NULL WHERE account_id=$1`, id); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}
