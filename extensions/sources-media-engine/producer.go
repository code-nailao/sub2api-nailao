package mediaengine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// Producer admits and reads durable tasks without owning their execution.
// It has no provider or billing boundary and never repairs interrupted work.
type Producer struct {
	store *Store
	opts  Options
}

func NewProducer(s *Store, o Options) (*Producer, error) {
	if s == nil || s.db == nil || s.closed.Load() {
		return nil, errors.New("open task store required")
	}
	if o.RecoveryPolicy != nil {
		policy := *o.RecoveryPolicy
		policy.LegacyTaskIDs = append([]string(nil), policy.LegacyTaskIDs...)
		if err := policy.Validate(); err != nil {
			return nil, err
		}
		for _, id := range policy.LegacyTaskIDs {
			if !s.ns.IsTaskID(id) {
				return nil, errors.New("invalid media engine historical recovery scope")
			}
		}
		if time.Duration(policy.TimeoutSeconds)*time.Second < defaults(o).GenerationTimeout {
			return nil, errors.New("recovery deadline must cover the generation budget")
		}
		o.RecoveryPolicy = &policy
	}
	return &Producer{store: s, opts: defaults(o)}, nil
}

func (p *Producer) Existing(o Owner, idem, hash string) (*Task, error) {
	t, err := p.store.Existing(o, idem)
	if err != nil {
		return nil, err
	}
	if t.PayloadHash != hash {
		return nil, ErrConflict
	}
	return t, nil
}

func (p *Producer) Create(in Input) (*Task, error) {
	return p.create(in, "")
}

func (p *Producer) create(in Input, holdToken string) (*Task, error) {
	// The public request fingerprint is independent of account selection and
	// refreshed input URLs. A retry must return the first durable admission.
	hash, err := hex.DecodeString(in.PayloadHash)
	if in.Owner.UserID <= 0 || in.Owner.KeyID <= 0 || in.Owner.GroupID <= 0 || in.Credential.AccountID <= 0 || len(in.IdempotencyKey) < 1 || len(in.IdempotencyKey) > 200 || len(in.Body) == 0 || len(in.Body) > 1<<20 || len(in.Ticket) > 1<<20 || !json.Valid(in.Body) || !json.Valid(in.Ticket) || err != nil || len(hash) != 32 {
		return nil, ErrConflict
	}
	random := make([]byte, 16)
	if _, err = rand.Read(random); err != nil {
		return nil, err
	}
	now := p.opts.Now()
	t := &Task{ID: p.store.ns.newTaskID(hex.EncodeToString(random)), Owner: in.Owner, AccountID: in.Credential.AccountID, IdempotencyKey: in.IdempotencyKey, PayloadHash: in.PayloadHash, Body: in.Body, Ticket: in.Ticket, Credential: in.Credential, Phase: "admitting", CreatedAt: now.Unix(), UpdatedAt: now.Unix(), NextRun: now.UnixMilli(), ReviewAfterAt: now.Add(p.opts.ReviewAfter).Unix()}
	if holdToken != "" {
		t.AdmissionHold = holdToken
		t.NextRun = admissionHoldNextRun(holdToken)
	}
	return p.store.admit(t, p.opts.MaxRecordsPerKey)
}

func (p *Producer) Get(o Owner, id string) (*Task, error) {
	t, err := p.store.Get(id)
	if err != nil || t.Owner != o {
		return nil, ErrNotFound
	}
	return t, nil
}

func (p *Producer) ReviewPage(phase, after string) ([]*Task, error) {
	if phase != "unknown" && phase != "delivery_review" {
		return nil, ErrConflict
	}
	return p.store.PhasePage(phase, after, 100)
}
