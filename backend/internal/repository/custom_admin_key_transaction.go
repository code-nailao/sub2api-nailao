package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
)

// accountTransactionScope 同时切换 ORM、原生 SQL 和关联查询，禁止在提交前发布缓存。
func (r *accountRepository) accountTransactionScope(ctx context.Context) *accountRepository {
	tx := dbent.TxFromContext(ctx)
	if tx == nil || r.client == tx.Client() {
		return r
	}
	scoped := *r
	scoped.client = tx.Client()
	scoped.sql = tx.Client()
	scoped.schedulerCache = nil
	return &scoped
}

type scopedOpenAIRefreshSessionKey struct{}

type scopedOpenAIRefreshSession struct {
	accountID int64
	conn      *sql.Conn
	reader    *dbent.Client
}

// WithScopedOpenAIRefreshLocks uses the same advisory keys and order as the
// scoped account write transaction. Session locks don't require a transaction
// to span the wait, scope recheck, and upstream OAuth exchange.
func (r *accountRepository) WithScopedOpenAIRefreshLocks(ctx context.Context, id int64, run func(context.Context) error) error {
	db, ok := r.sql.(*sql.DB)
	if !ok || db == nil {
		return fmt.Errorf("scoped OAuth refresh requires a PostgreSQL connection pool")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_, accountErr := conn.ExecContext(cleanup, `SELECT pg_advisory_unlock(hashtextextended('scoped_admin_account:' || $1::text, 0))`, id)
		_, configErr := conn.ExecContext(cleanup, `SELECT pg_advisory_unlock(hashtextextended('sources_request_speed_config', 0))`)
		if accountErr != nil || configErr != nil {
			// If an unlock outcome is uncertain, close the physical session
			// instead of allowing its locks to leak into the connection pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended('sources_request_speed_config', 0))`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended('scoped_admin_account:' || $1::text, 0))`, id); err != nil {
		return err
	}
	reader := dbent.NewClient(dbent.Driver(entsql.NewDriver(dialect.Postgres, entsql.Conn{ExecQuerier: conn})))
	session := &scopedOpenAIRefreshSession{accountID: id, conn: conn, reader: reader}
	return run(context.WithValue(ctx, scopedOpenAIRefreshSessionKey{}, session))
}

func (r *accountRepository) scopedOpenAIRefreshReader(ctx context.Context, id int64) *accountRepository {
	session, _ := ctx.Value(scopedOpenAIRefreshSessionKey{}).(*scopedOpenAIRefreshSession)
	if session == nil || session.accountID != id || r.client == session.reader {
		return r
	}
	scoped := *r
	scoped.client = session.reader
	scoped.sql = session.conn
	return &scoped
}
