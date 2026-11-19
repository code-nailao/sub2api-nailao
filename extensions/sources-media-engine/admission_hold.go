package mediaengine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"regexp"
	"time"
)

var admissionHoldToken = regexp.MustCompile(`^[a-f0-9]{32,64}$`)

const admissionHoldBatchSize = 100

// AdmissionHoldCount is a read-only barrier check for a repeated owner handoff.
// Count the entire sentinel bucket so corrupt metadata or a token collision
// blocks the next cycle instead of hiding unfinished work. No task is released.
func (p *Producer) AdmissionHoldCount(token string) (int, error) {
	if !admissionHoldToken.MatchString(token) {
		return 0, ErrConflict
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	err := p.store.db.QueryRowContext(ctx, "SELECT count(*) FROM tasks WHERE next_run=?", admissionHoldNextRun(token)).Scan(&count)
	return count, err
}

// admissionHoldNextRun reserves the far-future int64 range [2^62, 2^63).
// Token-specific sentinels use the existing (phase,next_run,id) index: recovery
// does not scan unrelated tokens or require a schema change on a legacy store.
// The full authenticated token is still checked; a hash collision fails closed.
func admissionHoldNextRun(token string) int64 {
	hash := sha256.Sum256([]byte(token))
	const base uint64 = 1 << 62
	return int64(base | (binary.BigEndian.Uint64(hash[:8]) & (base - 1)))
}

// CreateHeld durably admits a task without reserving money or starting work.
// Callers must first drain every legacy Create path. There is no automatic
// expiration: the coordinator persists the token and owns explicit recovery.
func (p *Producer) CreateHeld(in Input, token string) (*Task, error) {
	if !admissionHoldToken.MatchString(token) {
		return nil, ErrConflict
	}
	return p.create(in, token)
}

// ReleaseAdmissionHold makes at most 100 untouched admissions runnable. Both
// serving and the exclusive owner must supply the exact persisted token.
// Before calling, stop creating this hold token and drain in-flight admissions;
// then repeat until zero. No execution state, money or upstream work is changed.
// A restart or an unknown token never implicitly releases held work.
func (p *Producer) ReleaseAdmissionHold(token string) (int, error) {
	if !admissionHoldToken.MatchString(token) {
		return 0, ErrConflict
	}
	sentinel := admissionHoldNextRun(token)
	tx, err := p.store.db.BeginTx(context.Background(), nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT "+taskColumns+" FROM tasks WHERE phase='admitting' AND next_run=? ORDER BY id LIMIT ?", sentinel, admissionHoldBatchSize)
	if err != nil {
		return 0, err
	}
	var held []*Task
	for rows.Next() {
		t, scanErr := p.store.scan(rows)
		if scanErr != nil {
			_ = rows.Close()
			return 0, scanErr
		}
		if t.AdmissionHold != token || t.Phase != "admitting" || t.Version != 1 || t.NextRun != sentinel || t.UpstreamID != "" || t.Attempts != 0 || t.TerminalAt != 0 {
			_ = rows.Close()
			return 0, ErrConflict
		}
		held = append(held, t)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, err
	}
	now := p.opts.Now()
	for _, t := range held {
		t.AdmissionHold = ""
		t.NextRun = now.UnixMilli()
		t.UpdatedAt = now.Unix()
		if _, err := p.store.writeTask(tx, t, 0, "", ""); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(held), nil
}
