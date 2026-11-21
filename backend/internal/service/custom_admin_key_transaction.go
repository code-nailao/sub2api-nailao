package service

import (
	"context"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

func lockScopedAdminAccount(ctx context.Context, tx *dbent.Tx, id int64) error {
	// Keep scoped-admin outer transactions in the same configuration lock order.
	if _, err := tx.Client().ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('sources_request_speed_config', 0))"); err != nil {
		return err
	}
	_, err := tx.Client().ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('scoped_admin_account:' || $1::text, 0))`, id)
	return err
}

type scopedOpenAIRefreshLocker interface {
	WithScopedOpenAIRefreshLocks(context.Context, int64, func(context.Context) error) error
}

type scopedRefreshCommitHooksKey struct{}

// The session locks keep the checked scope stable during the exchange. The
// conditional credentials write has its own timeout and commits independently.
func (s *adminServiceImpl) refreshScopedOpenAIAccount(ctx context.Context, id int64, run func(context.Context) (*Account, error)) (*Account, error) {
	locker, ok := s.accountRepo.(scopedOpenAIRefreshLocker)
	if !ok {
		return nil, infraerrors.ServiceUnavailable("SCOPED_REFRESH_UNAVAILABLE", "Scoped account refresh is unavailable")
	}
	wait, cancelWait := context.WithTimeout(dbent.NewTxContext(ctx, nil), scopedAdminRefreshTransactionTimeout)
	defer cancelWait()
	var updated *Account
	var hooks []func(context.Context)
	if err := locker.WithScopedOpenAIRefreshLocks(wait, id, func(locked context.Context) error {
		if err := locked.Err(); err != nil {
			return err
		}
		work, cancel := context.WithTimeout(context.WithoutCancel(locked), scopedAdminRefreshTransactionTimeout)
		defer cancel()
		work = context.WithValue(work, scopedRefreshCommitHooksKey{}, &hooks)
		var err error
		updated, err = run(work)
		return err
	}); err != nil {
		return nil, err
	}
	// Run account side effects on the caller context after returning the
	// advisory-lock connection to the pool.
	for _, hook := range hooks {
		hook(ctx)
	}
	return updated, nil
}

// AfterAccountCommit 将不参与回滚的缓存和上游副作用延迟到事务提交成功后。
// 普通管理员没有外层事务时立即执行，保留原有行为。
func AfterAccountCommit(ctx context.Context, run func(context.Context)) {
	if tx := dbent.TxFromContext(ctx); tx != nil {
		tx.OnCommit(func(next dbent.Committer) dbent.Committer {
			return dbent.CommitFunc(func(commitCtx context.Context, tx *dbent.Tx) error {
				if err := next.Commit(commitCtx, tx); err != nil {
					return err
				}
				run(dbent.NewTxContext(ctx, nil))
				return nil
			})
		})
	} else if hooks, ok := ctx.Value(scopedRefreshCommitHooksKey{}).(*[]func(context.Context)); ok {
		*hooks = append(*hooks, run)
	} else {
		run(ctx)
	}
}
