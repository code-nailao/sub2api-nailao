package mediaengine

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

// Store owns an external SQLite database, never the Main business database.
// The legacy filesystem lock elects one engine owner; producers only admit work.
// SQL transactions serialize admission and versions fence stale engine writes.
// Indexed metadata is non-secret. Payloads, tickets and credentials use AEAD.
type Store struct {
	db            *sql.DB
	gcm           cipher.AEAD
	lock          *os.File
	owner         bool
	closed        atomic.Bool
	engineClaimed atomic.Bool
	closeOnce     sync.Once
	closeErr      error
	ns            Namespace
}

// Open initializes the store and acquires the same exclusive engine lock used
// by v0.2.28. Close the Engine before closing this Store to release ownership.
// ns must be the namespace the store was created under; a store created under
// a legacy name is refused without it, exactly like a wrong key.
func Open(dir string, key []byte, ns Namespace) (*Store, error) {
	return openStore(dir, key, ns, true)
}

// OpenProducer requires an already initialized store and the same encryption
// key. It neither acquires the engine lock nor repairs interrupted submissions.
func OpenProducer(dir string, key []byte, ns Namespace) (*Store, error) {
	return openStore(dir, key, ns, false)
}

func openStore(dir string, key []byte, ns Namespace, owner bool) (*Store, error) {
	if len(key) != 32 {
		return nil, errors.New("32-byte task encryption key required")
	}
	if owner {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("task directory must be owner-only")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s := &Store{gcm: gcm, owner: owner, ns: ns}
	fail := func(e error) (*Store, error) { _ = s.Close(); return nil, e }
	if owner {
		s.lock, err = os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return fail(err)
		}
		if err = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return fail(errors.New("task store already has an owner"))
		}
	}
	// This short lock covers schema initialization and identity verification only.
	// Producers never take the legacy .lock, which the engine holds for its lifetime.
	schemaLock, err := openSchemaLock(dir)
	if err != nil {
		return fail(err)
	}
	defer schemaLock.Close()
	dbPath := filepath.Join(dir, "tasks.sqlite")
	mode := "rwc"
	if !owner {
		mode = "rw"
		info, err := os.Stat(dbPath)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fail(errors.New("initialized owner-only task store required"))
		}
	}
	u := url.URL{Scheme: "file", Path: dbPath}
	query := "?mode=" + mode + "&_txlock=immediate&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if owner {
		query += "&_pragma=journal_mode(WAL)"
	}
	s.db, err = sql.Open("sqlite", u.String()+query)
	if err != nil {
		return fail(err)
	}
	s.db.SetMaxOpenConns(1)
	if owner {
		_, err = s.db.Exec(`CREATE TABLE IF NOT EXISTS store_metadata (id INTEGER PRIMARY KEY CHECK(id=1), version INTEGER NOT NULL, key_check BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS tasks(id TEXT PRIMARY KEY,user_id INTEGER NOT NULL,key_id INTEGER NOT NULL,group_id INTEGER NOT NULL,account_id INTEGER NOT NULL,idem_hash TEXT NOT NULL,payload_hash TEXT NOT NULL,phase TEXT NOT NULL,next_run INTEGER NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,terminal_at INTEGER NOT NULL DEFAULT 0,version INTEGER NOT NULL,record BLOB NOT NULL,UNIQUE(user_id,key_id,group_id,idem_hash));
 CREATE INDEX IF NOT EXISTS tasks_due ON tasks(phase,next_run,id);
 CREATE INDEX IF NOT EXISTS tasks_key_phase ON tasks(key_id,phase);
 CREATE INDEX IF NOT EXISTS tasks_account_phase ON tasks(account_id,phase);
 CREATE INDEX IF NOT EXISTS tasks_retention ON tasks(terminal_at,id);
 CREATE TABLE IF NOT EXISTS task_events(id INTEGER PRIMARY KEY,task_id TEXT NOT NULL,created_at INTEGER NOT NULL,operator_id INTEGER NOT NULL,action TEXT NOT NULL,detail BLOB NOT NULL);
 CREATE INDEX IF NOT EXISTS events_task ON task_events(task_id,id);
 CREATE TABLE IF NOT EXISTS upstream_claims(account_id INTEGER NOT NULL,reference_hash TEXT NOT NULL,task_id TEXT NOT NULL UNIQUE,PRIMARY KEY(account_id,reference_hash));`)
	} else {
		// Validate the existing schema without creating or changing any table.
		_, err = s.db.Exec("SELECT " + taskColumns + " FROM tasks LIMIT 0")
	}
	if err != nil {
		return fail(err)
	}
	var version int
	var check []byte
	err = s.db.QueryRow("SELECT version,key_check FROM store_metadata WHERE id=1").Scan(&version, &check)
	if errors.Is(err, sql.ErrNoRows) && owner {
		check, err = s.seal("key-check", []byte(ns.keyCheck()))
		if err == nil {
			_, err = s.db.Exec("INSERT INTO store_metadata VALUES(1,1,?)", check)
		}
	} else if err == nil {
		var raw []byte
		raw, err = s.open("key-check", check)
		if err == nil && (version != 1 || !ns.Matches(string(raw), keyCheckValue)) {
			err = errors.New("unsupported task store")
		}
	}
	if err != nil {
		return fail(errors.New("task store identity or key check failed"))
	}
	if owner {
		if err = os.Chmod(dbPath, 0600); err != nil {
			return fail(err)
		}
	}
	return s, nil
}

func openSchemaLock(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, ".schema.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("acquire task schema lock: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *Store) requireOwner() error {
	if s == nil || !s.owner || s.closed.Load() {
		return ErrNotOwner
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if s.db != nil {
			s.closeErr = s.db.Close()
		}
		if s.lock != nil {
			s.closeErr = errors.Join(s.closeErr, s.lock.Close())
		}
	})
	return s.closeErr
}
func digest(v string) string { sum := sha256.Sum256([]byte(v)); return hex.EncodeToString(sum[:]) }
func (s *Store) seal(aad string, raw []byte) ([]byte, error) {
	n := make([]byte, s.gcm.NonceSize())
	if _, err := rand.Read(n); err != nil {
		return nil, err
	}
	return s.gcm.Seal(n, n, raw, []byte(aad)), nil
}
func (s *Store) open(aad string, raw []byte) ([]byte, error) {
	n := s.gcm.NonceSize()
	if len(raw) <= n {
		return nil, errors.New("truncated encrypted task")
	}
	b, err := s.gcm.Open(nil, raw[:n], raw[n:], []byte(aad))
	if err != nil {
		return nil, errors.New("task authentication failed")
	}
	return b, nil
}
func clone(t *Task) *Task {
	raw, _ := json.Marshal(t)
	var out Task
	_ = json.Unmarshal(raw, &out)
	return &out
}

const taskColumns = "id,user_id,key_id,group_id,account_id,idem_hash,payload_hash,phase,next_run,created_at,updated_at,terminal_at,version,record"

type scanner interface{ Scan(...any) error }

func (s *Store) scan(row scanner) (*Task, error) {
	var id, idem, hash, phase string
	var user, key, group, account, next, created, updated, terminal, version int64
	var record []byte
	if err := row.Scan(&id, &user, &key, &group, &account, &idem, &hash, &phase, &next, &created, &updated, &terminal, &version, &record); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	raw, err := s.open(id+":"+strconv.FormatInt(version, 10), record)
	if err != nil {
		return nil, err
	}
	var t Task
	if json.Unmarshal(raw, &t) != nil || t.ID != id || t.Version != version || t.AccountID != account || t.Owner != (Owner{user, key, group}) || digest(t.IdempotencyKey) != idem || t.PayloadHash != hash || t.Phase != phase || t.NextRun != next || t.CreatedAt != created || t.UpdatedAt != updated || t.TerminalAt != terminal {
		return nil, errors.New("task index disagrees with encrypted record")
	}
	return &t, nil
}
func (s *Store) Get(id string) (*Task, error) {
	if !s.ns.IsTaskID(id) {
		return nil, ErrNotFound
	}
	return s.scan(s.db.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE id=?", id))
}
func (s *Store) Existing(o Owner, idem string) (*Task, error) {
	return s.scan(s.db.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE user_id=? AND key_id=? AND group_id=? AND idem_hash=?", o.UserID, o.KeyID, o.GroupID, digest(idem)))
}
func (s *Store) ActiveCount(keyID int64) (int, error) {
	var n int
	err := s.db.QueryRow("SELECT count(*) FROM tasks WHERE key_id=? AND phase NOT IN ('completed','failed','archived')", keyID).Scan(&n)
	return n, err
}
func (s *Store) page(where string, args ...any) ([]*Task, error) {
	rows, err := s.db.Query("SELECT "+taskColumns+" FROM tasks WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Task{}
	for rows.Next() {
		t, e := s.scan(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (s *Store) PhasePage(phase, after string, limit int) ([]*Task, error) {
	return s.page("phase=? AND id>? ORDER BY id LIMIT ?", phase, after, boundedLimit(limit))
}
func (s *Store) Due(now int64, limit int) ([]*Task, error) {
	// Releasing tasks use the dedicated bounded money lane, independently of
	// queued generation and without racing a second scheduled release attempt.
	return s.page("phase IN ('admitting','queued','submitting','running','downloading','settling') AND next_run<=? ORDER BY next_run,id LIMIT ?", now, boundedLimit(limit))
}
func boundedLimit(n int) int {
	if n < 1 {
		return 20
	}
	if n > 200 {
		return 200
	}
	return n
}

// admit serializes cross-process idempotency, capacity and insertion with the
// connection's BEGIN IMMEDIATE transaction. A Go mutex cannot protect another
// serving process, and a deferred read transaction cannot safely upgrade here.
func (s *Store) admit(t *Task, maxRecords int) (*Task, error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	existing, err := s.scan(tx.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE user_id=? AND key_id=? AND group_id=? AND idem_hash=?", t.Owner.UserID, t.Owner.KeyID, t.Owner.GroupID, digest(t.IdempotencyKey)))
	if err == nil {
		if existing.PayloadHash != t.PayloadHash {
			return nil, ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	var active int
	if err = tx.QueryRow("SELECT count(*) FROM tasks WHERE key_id=? AND phase NOT IN ('completed','failed','archived')", t.Owner.KeyID).Scan(&active); err != nil {
		return nil, err
	}
	if active >= maxRecords {
		return nil, ErrCapacity
	}
	next, err := s.writeTask(tx, t, 0, "", "")
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return next, nil
}

func (s *Store) Save(t *Task) error { return s.saveWithEvent(t, 0, "", "") }
func (s *Store) saveWithEvent(t *Task, operator int64, action, detail string) error {
	if err := s.requireOwner(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	next, err := s.writeTask(tx, t, operator, action, detail)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	*t = *next
	return nil
}

func (s *Store) writeTask(tx *sql.Tx, t *Task, operator int64, action, detail string) (*Task, error) {
	if !s.ns.IsTaskID(t.ID) {
		return nil, ErrConflict
	}
	next := clone(t)
	next.Version++
	raw, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	record, err := s.seal(next.ID+":"+strconv.FormatInt(next.Version, 10), raw)
	if err != nil {
		return nil, err
	}
	// Async provider concurrency is a durable job lease, not HTTP request count
	// or per-user record capacity. Unknown submissions retain their lease until
	// reconciliation; succeeded jobs release it before local delivery retries.
	if next.Phase == "submitting" && next.Credential.Concurrency > 0 {
		var active int
		err = tx.QueryRow("SELECT COUNT(*) FROM tasks WHERE account_id=? AND id<>? AND phase IN ('submitting','running','unknown')", next.AccountID, next.ID).Scan(&active)
		if err != nil {
			return nil, err
		}
		if active >= next.Credential.Concurrency {
			return nil, ErrDeferred
		}
	}
	values := []any{next.Owner.UserID, next.Owner.KeyID, next.Owner.GroupID, next.AccountID, digest(next.IdempotencyKey), next.PayloadHash, next.Phase, next.NextRun, next.CreatedAt, next.UpdatedAt, next.TerminalAt, next.Version, record}
	if t.Version == 0 {
		_, err = tx.Exec("INSERT INTO tasks("+taskColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)", append([]any{next.ID}, values...)...)
	} else {
		var result sql.Result
		result, err = tx.Exec("UPDATE tasks SET user_id=?,key_id=?,group_id=?,account_id=?,idem_hash=?,payload_hash=?,phase=?,next_run=?,created_at=?,updated_at=?,terminal_at=?,version=?,record=? WHERE id=? AND version=?", append(values, t.ID, t.Version)...)
		if err == nil {
			var n int64
			n, err = result.RowsAffected()
			if err == nil && n != 1 {
				err = ErrConflict
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if next.UpstreamID != "" {
		_, err = tx.Exec("INSERT INTO upstream_claims(account_id,reference_hash,task_id) VALUES(?,?,?) ON CONFLICT(task_id) DO NOTHING", next.AccountID, digest(next.UpstreamID), next.ID)
		if err != nil {
			return nil, fmt.Errorf("upstream reference already bound: %w", ErrConflict)
		}
		var account int64
		var ref string
		if err = tx.QueryRow("SELECT account_id,reference_hash FROM upstream_claims WHERE task_id=?", next.ID).Scan(&account, &ref); err != nil {
			return nil, err
		}
		if account != next.AccountID || ref != digest(next.UpstreamID) {
			return nil, ErrConflict
		}
	}
	if action != "" {
		sealed, e := s.seal("event:"+t.ID, []byte(detail))
		if e != nil {
			return nil, e
		}
		_, err = tx.Exec("INSERT INTO task_events(task_id,created_at,operator_id,action,detail) VALUES(?,?,?,?,?)", t.ID, time.Now().Unix(), operator, action, sealed)
		if err != nil {
			return nil, err
		}
	}
	return next, nil
}

// Archive terminal payloads only. Idempotency tombstones survive for 30 days;
// unresolved jobs and money tickets are never removed by time alone.
func (s *Store) Retain(now time.Time) error {
	if err := s.requireOwner(); err != nil {
		return err
	}
	// Platform-funded unknown closures retain their minimal original money ticket
	// and supplier reference until reconciliation. The terminal transition already
	// erased generation bodies and provider credentials; ordinary retention must
	// not erase the only evidence that supplier cost remains unknown.
	rows, err := s.page("phase IN ('completed','failed') AND terminal_at>0 AND terminal_at<? AND NOT EXISTS (SELECT 1 FROM task_events e WHERE e.task_id=tasks.id AND e.action IN ('auto_release_unknown','auto_release_historical_unknown')) ORDER BY terminal_at,id LIMIT 100", now.Add(-7*24*time.Hour).Unix())
	if err != nil {
		return err
	}
	for _, t := range rows {
		t.Phase = "archived"
		t.Body = nil
		t.Ticket = nil
		t.Credential = Credential{}
		t.SourceURLs = nil
		t.Result = nil
		if err = s.Save(t); err != nil {
			return err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec("DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE phase='archived' AND terminal_at<? ORDER BY terminal_at LIMIT 100)", now.Add(-30*24*time.Hour).Unix())
	if err != nil {
		return err
	}
	_, err = tx.Exec("DELETE FROM upstream_claims WHERE task_id IN (SELECT id FROM tasks WHERE phase='archived' AND terminal_at<? ORDER BY terminal_at LIMIT 100)", now.Add(-30*24*time.Hour).Unix())
	if err != nil {
		return err
	}
	_, err = tx.Exec("DELETE FROM tasks WHERE id IN (SELECT id FROM tasks WHERE phase='archived' AND terminal_at<? ORDER BY terminal_at LIMIT 100)", now.Add(-30*24*time.Hour).Unix())
	if err != nil {
		return err
	}
	return tx.Commit()
}
