package repository

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 仅剥离已确认不参与余额请求的模型元数据字段。未知字段仍属于身份，不能凭
// “URL/Key 看起来一样”自动延续绑定（请求头、账户模式及协议均完整保留）。
func walletEditCallingIdentity(a *service.Account) string {
	copy := *a
	copy.Credentials = copyJSONMap(a.Credentials)
	for _, key := range []string{"model_mapping", "compact_model_mapping", "openai_capabilities"} {
		delete(copy.Credentials, key)
	}
	return service.AccountWalletIdentity(&copy)
}

// 在账号行已加锁的编辑事务中读取旧值，而不是相信编辑表单/服务层的旧快照。
func lockedWalletEditAccount(ctx context.Context, client *dbent.Client, after *service.Account) (*service.Account, error) {
	if after.Type != service.AccountTypeAPIKey || after.ParentAccountID != nil {
		return nil, nil
	}
	row, err := client.Account.Get(ctx, after.ID)
	if err != nil {
		return nil, err
	}
	before := accountEntityToService(row)
	if before.Type != service.AccountTypeAPIKey || before.ParentAccountID != nil {
		return nil, nil
	}
	// 新旧代理 ID 不一致时无需延续。代理独立编辑不反向加锁，避免与代理→账号
	// 的既有锁顺序形成环；旧绑定 CAS 仍包含下方读取的完整代理 URL。
	if !reflect.DeepEqual(before.ProxyID, after.ProxyID) {
		return nil, nil
	}
	if before.ProxyID != nil {
		proxy, err := client.Proxy.Get(ctx, *before.ProxyID)
		if dbent.IsNotFound(err) {
			return nil, nil // 已移除的代理不延续绑定，也不阻止普通账号编辑。
		}
		if err != nil {
			return nil, err
		}
		before.Proxy = proxyEntityToService(proxy)
	}
	return before, nil
}

// 不改既有哈希格式：既有读者仍能按完整凭据哈希读取。只有调用身份
// 精确不变时，把精确命中的旧绑定/快照原子转写到保存后的新哈希。钱包归属、
// 金额、提醒、成员版本与正在执行的租约全部保持；旧请求的完成 CAS 会拒绝旧哈希。
func preserveWalletAfterMetadataEdit(ctx context.Context, client *dbent.Client, before, after *service.Account) error {
	if before == nil {
		return nil
	}
	next := *after
	// Update 的 DTO 常不带关联代理；代理 ID 已确认相同，使用事务内读取的值。
	// 若代理并发改动，完整哈希仍与新代理不符，后续读者会要求重新确认。
	next.Proxy = before.Proxy
	if walletEditCallingIdentity(before) != walletEditCallingIdentity(&next) {
		return nil
	}
	oldIdentity, newIdentity := service.AccountWalletIdentity(before), service.AccountWalletIdentity(&next)
	if oldIdentity == newIdentity {
		return nil
	}
	path, err := json.Marshal([]string{"members", strconv.FormatInt(after.ID, 10), "identity"})
	if err != nil {
		return err
	}
	// 钱包的共享注册表忙时须有限退出，不能让普通账号编辑无限等待。
	writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err = client.ExecContext(writeCtx, `UPDATE sources_wallet_management
		SET value=jsonb_set(value, ARRAY(SELECT jsonb_array_elements_text($1::jsonb)), to_jsonb($2::text), false), updated_at=NOW()
		WHERE id=1 AND value->'members'->($3::text)->>'identity'=$4
		AND coalesce(value->'members'->($3::text)->>'wallet_id','')<>''`, string(path), newIdentity, strconv.FormatInt(after.ID, 10), oldIdentity)
	if err != nil {
		return err
	}
	_, err = client.ExecContext(writeCtx, `UPDATE sources_account_wallet_snapshots
		SET identity=$1, updated_at=NOW() WHERE account_id=$2 AND identity=$3
		AND NOT EXISTS (SELECT 1 FROM sources_wallet_management m WHERE m.id=1
			AND coalesce(m.value->'members'->($2::text)->>'wallet_id','')<>''
			AND m.value->'members'->($2::text)->>'identity' IS DISTINCT FROM $1::text)`, newIdentity, after.ID, oldIdentity)
	return err
}
