package portal

import (
	"context"
	"regexp"
	"strconv"
	"time"
)

type balanceAsyncJob struct {
	result  Source
	err     error
	done    bool
	expires time.Time
}

var balanceRequestIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,64}$`)

func validBalanceRequestID(id string) bool { return balanceRequestIDPattern.MatchString(id) }

func (s *Service) RefreshAsync(ctx context.Context, id, requestID string) (Source, error) {
	if !validBalanceRequestID(requestID) {
		return Source{}, ErrConflict
	}
	if ctx.Err() != nil {
		return Source{}, ctx.Err()
	}
	rec, ok := s.Store.snapshot()[id]
	if !ok {
		return Source{}, ErrNotFound
	}
	key := id + ":" + strconv.FormatInt(rec.Source.Revision, 10) + ":" + requestID
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.asyncClosing {
		return Source{}, ErrTransport
	}
	for key, job := range s.asyncJobs {
		if job.done && time.Now().After(job.expires) {
			delete(s.asyncJobs, key)
		}
	}
	if job, ok := s.asyncJobs[key]; ok {
		return job.result, job.err
	}
	if s.asyncActive >= balanceConcurrency || len(s.asyncJobs) >= 512 {
		return Source{}, ErrBusy
	}
	pending := rec.Source
	pending.Querying = true
	job := &balanceAsyncJob{result: pending}
	s.asyncJobs[key] = job
	s.asyncActive++
	s.asyncWorkers.Add(1)
	go func() {
		defer s.asyncWorkers.Done()
		call, cancel := context.WithTimeout(s.asyncCtx, balanceOperationTimeout)
		defer cancel()
		result, err := s.Refresh(call, id)
		if err == nil && result.Revision != rec.Source.Revision {
			result, err = Source{}, ErrConflict
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		job.result, job.err, job.done, job.expires = result, err, true, time.Now().Add(10*time.Minute)
		s.asyncActive--
	}()
	return pending, nil
}

func (s *Service) StopAsync() {
	s.mu.Lock()
	s.asyncClosing = true
	s.asyncCancel()
	s.mu.Unlock()
	s.asyncWorkers.Wait()
}
