//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestWalletModelRefreshSavePreservesLegacyReaderAndCAS(t *testing.T) {
	ctx := context.Background()
	repo := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	var original []byte
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT value FROM sources_wallet_management WHERE id=1`).Scan(&original))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `UPDATE sources_wallet_management SET value=$1::jsonb WHERE id=1`, string(original))
	})
	a := walletEditFixture()
	a.ID = 0
	a.Name, a.Status, a.Concurrency = "wallet-model-refresh", "active", 1
	require.NoError(t, repo.Create(ctx, a))
	t.Cleanup(func() { _ = testEntClient(t).Account.DeleteOneID(a.ID).Exec(ctx) })
	a, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	config := map[string]any{"url": "https://balance.example", "version": "verified-fixture"}
	require.NoError(t, repo.UpdateAccountWalletConfig(ctx, a, config))
	a, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	oldID := service.AccountWalletIdentity(a)
	registry := &walletRegistryRepository{db: integrationDB}
	require.NoError(t, registry.Change(ctx, func(s *service.WalletRegistry) error {
		s.Profiles["sub:model-refresh"] = service.ManagedWallet{ID: "sub:model-refresh", Revision: 3, Provider: "sub", RepresentativeID: a.ID, Alert: service.WalletAlertRule{Threshold: "12", Unit: "USD"}}
		s.Members[a.ID] = service.WalletMember{WalletID: "sub:model-refresh", Identity: oldID, Revision: 7}
		return nil
	}))
	snapshots := &accountWalletSnapshotRepository{sql: integrationDB}
	now, next := time.Now().UTC(), time.Now().UTC().Add(5*time.Minute)
	ok, err := snapshots.Claim(ctx, a.ID, oldID, "initial", false)
	require.NoError(t, err)
	require.True(t, ok)
	value := service.AccountWallet{Status: "ready", Remaining: "12.345678901", Unit: "USD", CheckedAt: &now, NextCheckAt: &next, AutoRefresh: true}
	ok, err = snapshots.Complete(ctx, a.ID, oldID, "initial", value)
	require.NoError(t, err)
	require.True(t, ok)
	// 模型同步接口仅保存能力元数据，原来就不该失效。
	require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{service.UpstreamModelMetadataExtraKey: map[string]any{"synced_at": "fixture"}}))
	a, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, oldID, service.AccountWalletIdentity(a))
	// 真实编辑保存路径（旧表单不含明文Key，由service已合并保留）改变model_mapping。
	a.Credentials["model_mapping"] = map[string]any{"old": "old", "new": "new"}
	require.NoError(t, repo.UpdateWithAccountBillingSettings(ctx, a, nil, nil, nil))
	a, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	newID := service.AccountWalletIdentity(a)
	require.NotEqual(t, oldID, newID, "keep existing full-hash formula")
	state, err := registry.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, service.WalletMember{WalletID: "sub:model-refresh", Identity: newID, Revision: 7}, state.Members[a.ID])
	require.EqualValues(t, 3, state.Profiles["sub:model-refresh"].Revision)
	require.Equal(t, config, a.Extra[service.AccountWalletConfigExtraKey])
	require.Equal(t, service.WalletAlertRule{Threshold: "12", Unit: "USD"}, state.Profiles["sub:model-refresh"].Alert)
	row, err := snapshots.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, newID, row.Identity)
	require.Equal(t, value.Remaining, row.Value.Remaining)
	require.Equal(t, value, row.Value)
	manager := &service.WalletManager{Repo: registry, Accounts: repo}
	_, wallet, err := manager.Resolve(ctx, a)
	require.NoError(t, err, "existing reader resolves refreshed model account")
	require.Equal(t, "sub:model-refresh", wallet.ID)

	// 旧后台在途查询和模型保存并发：保存持有snapshot行锁，旧身份完成CAS
	// 必须在提交后拒绝，不能覆盖已保留的余额；Release仍可正常释放原租约。
	_, err = integrationDB.ExecContext(ctx, `UPDATE sources_account_wallet_snapshots SET last_started_at=NOW()-INTERVAL '1 minute' WHERE account_id=$1`, a.ID)
	require.NoError(t, err)
	ok, err = snapshots.Claim(ctx, a.ID, newID, "old-reader-flight", false)
	require.NoError(t, err)
	require.True(t, ok)
	concurrentTx, err := testEntClient(t).Tx(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = concurrentTx.Rollback() })
	a.Credentials["model_mapping"] = map[string]any{"concurrent": "concurrent"}
	require.NoError(t, repo.UpdateWithAccountBillingSettings(dbent.NewTxContext(ctx, concurrentTx), a, nil, nil, nil))
	type completion struct {
		ok  bool
		err error
	}
	done := make(chan completion, 1)
	go func() {
		finishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		written, finishErr := snapshots.Complete(finishCtx, a.ID, newID, "old-reader-flight", service.AccountWallet{Status: "ready", Remaining: "999"})
		done <- completion{written, finishErr}
	}()
	select {
	case result := <-done:
		t.Fatalf("old completion bypassed uncommitted account/snapshot edit: %+v", result)
	case <-time.After(100 * time.Millisecond):
	}
	lateClaim := make(chan completion, 1)
	go func() {
		claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		claimed, claimErr := snapshots.Claim(claimCtx, a.ID, newID, "queued-old-reader", false)
		lateClaim <- completion{claimed, claimErr}
	}()
	select {
	case result := <-lateClaim:
		t.Fatalf("claim bypassed uncommitted account edit: %+v", result)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, concurrentTx.Commit())
	claimResult := <-lateClaim
	require.NoError(t, claimResult.err)
	require.False(t, claimResult.ok)
	result := <-done
	require.NoError(t, result.err)
	require.False(t, result.ok)
	a, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	newID = service.AccountWalletIdentity(a)
	row, err = snapshots.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, newID, row.Identity)
	require.Equal(t, value.Remaining, row.Value.Remaining)
	require.True(t, row.InFlight)
	require.NoError(t, snapshots.Release(ctx, a.ID, "old-reader-flight"))
	row, err = snapshots.Get(ctx, a.ID)
	require.NoError(t, err)
	require.False(t, row.InFlight)
	_, _, err = manager.Resolve(ctx, a)
	require.NoError(t, err)

	// 编辑后晚到的旧手动刷新不能把快照身份改回去、清空余额。
	_, err = integrationDB.ExecContext(ctx, `UPDATE sources_account_wallet_snapshots SET last_started_at=NOW()-INTERVAL '1 minute' WHERE account_id=$1`, a.ID)
	require.NoError(t, err)
	ok, err = snapshots.Claim(ctx, a.ID, oldID, "late-old-reader", false)
	require.NoError(t, err)
	require.False(t, ok)
	row, err = snapshots.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, newID, row.Identity)
	require.Equal(t, value.Remaining, row.Value.Remaining)
	// 新身份仍可刷新并保留自动查询计划。
	ok, err = snapshots.Claim(ctx, a.ID, newID, "next-refresh", false)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = snapshots.Complete(ctx, a.ID, newID, "next-refresh", value)
	require.NoError(t, err)
	require.True(t, ok)

	// 数据库在第二个identity写入处真实报错，整个正常保存事务自动回滚。
	_, err = integrationDB.ExecContext(ctx, `CREATE FUNCTION wallet_edit_fail_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'wallet fixture snapshot rejected'; END $$;
		CREATE TRIGGER wallet_edit_fail_fixture BEFORE UPDATE ON sources_account_wallet_snapshots FOR EACH ROW EXECUTE FUNCTION wallet_edit_fail_fixture()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DROP TRIGGER IF EXISTS wallet_edit_fail_fixture ON sources_account_wallet_snapshots; DROP FUNCTION IF EXISTS wallet_edit_fail_fixture()`)
	})
	a.Credentials["model_mapping"] = map[string]any{"rejected": "rejected"}
	require.ErrorContains(t, repo.UpdateWithAccountBillingSettings(ctx, a, nil, nil, nil), "wallet fixture snapshot rejected")
	_, err = integrationDB.ExecContext(ctx, `DROP TRIGGER wallet_edit_fail_fixture ON sources_account_wallet_snapshots; DROP FUNCTION wallet_edit_fail_fixture()`)
	require.NoError(t, err)
	a, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, newID, service.AccountWalletIdentity(a))
	state, err = registry.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, newID, state.Members[a.ID].Identity)
	row, err = snapshots.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, newID, row.Identity)

	// 任一事务后续步骤失败时，账号与两处identity一并回滚。
	tx, err := testEntClient(t).Tx(ctx)
	require.NoError(t, err)
	a.Credentials["model_mapping"] = map[string]any{"rollback": "rollback"}
	require.NoError(t, repo.UpdateWithAccountBillingSettings(dbent.NewTxContext(ctx, tx), a, nil, nil, nil))
	require.NoError(t, tx.Rollback())
	current, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, newID, service.AccountWalletIdentity(current))
	state, err = registry.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, newID, state.Members[a.ID].Identity)

	// 已存在的失配绑定不可被“刷新模型”静默治愈；来源仍是原值。
	require.NoError(t, registry.Change(ctx, func(s *service.WalletRegistry) error {
		m := s.Members[a.ID]
		m.Identity = "unverified-old-identity"
		s.Members[a.ID] = m
		return nil
	}))
	current.Credentials["model_mapping"] = map[string]any{"third": "third"}
	require.NoError(t, repo.UpdateWithAccountBillingSettings(ctx, current, nil, nil, nil))
	state, err = registry.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, "unverified-old-identity", state.Members[a.ID].Identity)
	row, err = snapshots.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, newID, row.Identity, "mismatched member must not move snapshot toward another binding")
	_, _, err = manager.Resolve(ctx, current)
	require.True(t, errors.Is(err, service.ErrWalletRebind))

	// 真实Key变化同样不续接，snapshot不会跟新凭据走。
	require.NoError(t, registry.Change(ctx, func(s *service.WalletRegistry) error {
		m := s.Members[a.ID]
		m.Identity = service.AccountWalletIdentity(current)
		s.Members[a.ID] = m
		return nil
	}))
	bound := service.AccountWalletIdentity(current)
	current.Credentials["api_key"] = "rotated-key"
	require.NoError(t, repo.UpdateWithAccountBillingSettings(ctx, current, nil, nil, nil))
	state, err = registry.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, bound, state.Members[a.ID].Identity)
	_, _, err = manager.Resolve(ctx, current)
	require.ErrorIs(t, err, service.ErrWalletRebind)
	// 未绑定账号可以保留自身余额快照，但绝不创建成员或关联到其他钱包。
	require.NoError(t, registry.Change(ctx, func(s *service.WalletRegistry) error {
		delete(s.Members, a.ID)
		return nil
	}))
	row, err = snapshots.Get(ctx, a.ID)
	require.NoError(t, err)
	require.NoError(t, snapshots.Discard(ctx, a.ID, row.Identity))
	unboundID := service.AccountWalletIdentity(current)
	ok, err = snapshots.Claim(ctx, a.ID, unboundID, "unbound-fixture", false)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = snapshots.Complete(ctx, a.ID, unboundID, "unbound-fixture", value)
	require.NoError(t, err)
	require.True(t, ok)
	current.Credentials["model_mapping"] = map[string]any{"unbound": "unbound"}
	require.NoError(t, repo.UpdateWithAccountBillingSettings(ctx, current, nil, nil, nil))
	state, err = registry.Read(ctx)
	require.NoError(t, err)
	require.Empty(t, state.Members[a.ID].WalletID)
	row, err = snapshots.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, service.AccountWalletIdentity(current), row.Identity)
	require.Equal(t, value.Remaining, row.Value.Remaining)

	// 共享注册表被其他事务占用时，保存会在3秒上限退出且账号不发生半写入。
	beforeBlockedID := service.AccountWalletIdentity(current)
	require.NoError(t, registry.Change(ctx, func(s *service.WalletRegistry) error {
		s.Members[a.ID] = service.WalletMember{WalletID: "sub:model-refresh", Identity: beforeBlockedID, Revision: 7}
		return nil
	}))
	blockedTx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blockedTx.Rollback() })
	_, err = blockedTx.ExecContext(ctx, `SELECT value FROM sources_wallet_management WHERE id=1 FOR UPDATE`)
	require.NoError(t, err)
	current.Credentials["model_mapping"] = map[string]any{"blocked": "blocked"}
	start := time.Now()
	require.Error(t, repo.UpdateWithAccountBillingSettings(ctx, current, nil, nil, nil))
	require.Less(t, time.Since(start), 5*time.Second)
	require.NoError(t, blockedTx.Rollback())
	current, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, beforeBlockedID, service.AccountWalletIdentity(current))
	state, err = registry.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, beforeBlockedID, state.Members[a.ID].Identity)

	// 输出只包含脱敏结果，不记录密钥。
	raw, err := json.Marshal(state.Members[a.ID])
	require.NoError(t, err)
	require.NotContains(t, string(raw), "rotated-key")
	t.Log("wallet model refresh/legacy reader/rollback/CAS/real credential rebind verified account=" + strconv.FormatInt(a.ID, 10))
}

func TestWalletMetadataEditIdentityBoundaries(t *testing.T) {
	ctx := context.Background()
	repo := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, nil)
	registry := &walletRegistryRepository{db: integrationDB}
	var original []byte
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT value FROM sources_wallet_management WHERE id=1`).Scan(&original))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `UPDATE sources_wallet_management SET value=$1::jsonb WHERE id=1`, string(original))
	})
	for _, tc := range []struct {
		name     string
		preserve bool
		edit     func(*service.Account)
	}{
		{"mapping", true, func(a *service.Account) { a.Credentials["model_mapping"] = map[string]any{"new": "new"} }},
		{"remove_mapping", true, func(a *service.Account) { delete(a.Credentials, "model_mapping") }},
		{"compact_mapping", true, func(a *service.Account) { a.Credentials["compact_model_mapping"] = map[string]any{"new": "new"} }},
		{"capabilities", true, func(a *service.Account) { a.Credentials["openai_capabilities"] = map[string]any{"responses": true} }},
		{"key", false, func(a *service.Account) { a.Credentials["api_key"] = "changed" }},
		{"url", false, func(a *service.Account) { a.Credentials["base_url"] = "https://changed.example" }},
		{"headers", false, func(a *service.Account) { a.Credentials["header_overrides"] = map[string]any{"X-Tenant": "changed"} }},
		{"proxy_removed", false, func(a *service.Account) { a.ProxyID = nil }},
		{"protocol", false, func(a *service.Account) { a.Credentials["api_protocol"] = "changed" }},
		{"unknown", false, func(a *service.Account) { a.Credentials["future_field"] = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := walletEditFixture()
			proxy, err := testEntClient(t).Proxy.Create().SetName("wallet-proxy-" + tc.name).
				SetProtocol("http").SetHost("proxy.example").SetPort(8080).
				SetUsername("fixture").SetPassword("fixture-only").SetStatus("active").Save(ctx)
			require.NoError(t, err)
			t.Cleanup(func() { _ = testEntClient(t).Proxy.DeleteOneID(proxy.ID).Exec(ctx) })
			a.ProxyID = &proxy.ID
			a.ID = 0
			a.Name, a.Status, a.Concurrency = "wallet-boundary-"+tc.name, "active", 1
			require.NoError(t, repo.Create(ctx, a))
			t.Cleanup(func() { _ = testEntClient(t).Account.DeleteOneID(a.ID).Exec(ctx) })
			a, err = repo.GetByID(ctx, a.ID)
			require.NoError(t, err)
			config := map[string]any{"url": "https://balance.example", "version": "verified-fixture"}
			require.NoError(t, repo.UpdateAccountWalletConfig(ctx, a, config))
			a, err = repo.GetByID(ctx, a.ID)
			require.NoError(t, err)
			oldID := service.AccountWalletIdentity(a)
			walletID := "sub:boundary-" + tc.name
			require.NoError(t, registry.Change(ctx, func(s *service.WalletRegistry) error {
				s.Profiles[walletID] = service.ManagedWallet{ID: walletID, Provider: "sub", RepresentativeID: a.ID}
				s.Members[a.ID] = service.WalletMember{WalletID: walletID, Identity: oldID, Revision: 7}
				return nil
			}))
			snapshots := &accountWalletSnapshotRepository{sql: integrationDB}
			ok, err := snapshots.Claim(ctx, a.ID, oldID, "fixture", false)
			require.NoError(t, err)
			require.True(t, ok)
			now, next := time.Now().UTC(), time.Now().UTC().Add(5*time.Minute)
			value := service.AccountWallet{Status: "ready", Remaining: "12", Unit: "USD", CheckedAt: &now, NextCheckAt: &next, AutoRefresh: true}
			ok, err = snapshots.Complete(ctx, a.ID, oldID, "fixture", value)
			require.NoError(t, err)
			require.True(t, ok)
			// A stale form must not replace the verified wallet configuration or
			// metadata refreshed after the form was opened.
			a.Extra = map[string]any{service.AccountWalletConfigExtraKey: map[string]any{"url": "https://injected.example"}}
			require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{service.UpstreamModelMetadataExtraKey: map[string]any{"synced_at": "latest"}}))
			tc.edit(a)
			require.NoError(t, repo.Update(ctx, a))
			a, err = repo.GetByID(ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, config, a.Extra[service.AccountWalletConfigExtraKey])
			require.Equal(t, map[string]any{"synced_at": "latest"}, a.Extra[service.UpstreamModelMetadataExtraKey])
			state, err := registry.Read(ctx)
			require.NoError(t, err)
			row, err := snapshots.Get(ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, value, row.Value)
			manager := &service.WalletManager{Repo: registry, Accounts: repo}
			_, _, resolveErr := manager.Resolve(ctx, a)
			if tc.preserve {
				require.NoError(t, resolveErr)
				require.Equal(t, service.AccountWalletIdentity(a), state.Members[a.ID].Identity)
				require.Equal(t, service.AccountWalletIdentity(a), row.Identity)
			} else {
				require.ErrorIs(t, resolveErr, service.ErrWalletRebind)
				require.Equal(t, oldID, state.Members[a.ID].Identity)
				require.Equal(t, oldID, row.Identity)
			}
		})
	}
}
