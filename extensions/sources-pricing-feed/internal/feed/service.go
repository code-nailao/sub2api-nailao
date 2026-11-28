package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	PollInterval = 5 * time.Minute
	MaxStale     = 30 * time.Minute
	maxBody      = 8 << 20
)

type snapshot struct {
	Config   string   `json:"config_fingerprint"`
	Response Response `json:"response"`
}

type Service struct {
	config Config
	dir    string
	lock   *os.File
	client *http.Client
	logger *slog.Logger
	now    func() time.Time

	syncMu    sync.Mutex
	mu        sync.RWMutex
	body      []byte
	updatedAt time.Time
}

func New(c Config, dir string, logger *slog.Logger) (*Service, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "feed.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open process lock: %w", err)
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("state directory already in use")
	}
	if logger == nil {
		logger = slog.Default()
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = 2
	transport.MaxIdleConnsPerHost = 2
	transport.ResponseHeaderTimeout = 8 * time.Second
	s := &Service{config: c, dir: dir, lock: f, logger: logger, now: time.Now,
		client: &http.Client{Timeout: 12 * time.Second, Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	if err := s.restore(); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Warn("snapshot_restore_failed")
	}
	return s, nil
}

// Close must follow worker/server shutdown so the process lock covers all writes.
func (s *Service) Close() error {
	s.client.CloseIdleConnections()
	return s.lock.Close()
}

func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	for {
		if err := s.Sync(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger.Warn("price_sync_failed", "reason", err.Error())
		} else {
			s.logger.Info("price_sync_succeeded")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) Sync(ctx context.Context) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.config.SourceURL, nil)
	if err != nil {
		return fmt.Errorf("source request invalid")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", "Sources-Pricing-Feed/1.0")
	r, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("source request failed")
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return fmt.Errorf("source returned HTTP %d", r.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return fmt.Errorf("source body unreadable or too large")
	}
	var source plazaResponse
	if json.Unmarshal(body, &source) != nil {
		return fmt.Errorf("source body is not valid JSON")
	}
	converted, err := convert(s.config, source, s.now())
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return fmt.Errorf("source request cancelled")
	}
	stored := snapshot{Config: s.config.fingerprint(), Response: converted}
	if err := s.persist(stored); err != nil {
		return fmt.Errorf("snapshot persistence failed")
	}
	s.publish(converted)
	return nil
}

func (s *Service) publish(response Response) {
	body, _ := json.Marshal(response)
	s.mu.Lock()
	s.body, s.updatedAt = body, response.Data.UpdatedAt
	s.mu.Unlock()
}

func (s *Service) current() ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	age := s.now().Sub(s.updatedAt)
	return s.body, len(s.body) > 0 && age >= -time.Second && age <= MaxStale
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if _, ok := s.current(); !ok {
			http.Error(w, "price snapshot unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("GET /api/provider/pricing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		body, ok := s.current()
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(Response{Version: "1.1", Success: false, Message: "Price snapshot temporarily unavailable"})
			return
		}
		w.Write(body)
	})
	return mux
}

func (s *Service) persist(value snapshot) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".snapshot-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(s.dir, "snapshot.json")); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Service) restore() error {
	f, err := os.Open(filepath.Join(s.dir, "snapshot.json"))
	if err != nil {
		return err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, maxBody+1))
	if err != nil || len(body) > maxBody {
		return fmt.Errorf("invalid stored snapshot")
	}
	var value snapshot
	if json.Unmarshal(body, &value) != nil || value.Config != s.config.fingerprint() {
		return fmt.Errorf("invalid or incompatible snapshot")
	}
	r := value.Response
	if r.Version != "1.1" || !r.Success || r.Data == nil || r.Data.Currency != "CNY" || r.Data.PriceUnit != "per_1m_tokens" || r.Data.SiteName != s.config.SiteName || r.Data.Domain != s.config.SiteDomain || r.Data.UpdatedAt.IsZero() {
		return fmt.Errorf("invalid stored response")
	}
	wanted := map[string]bool{}
	for _, q := range s.config.Quotes {
		for _, model := range q.Models {
			wanted[q.GroupName+"\x00"+model] = true
		}
	}
	for _, m := range r.Data.Models {
		key := m.Group + "\x00" + m.Name
		p, err := number(m.Input)
		if !wanted[key] || !m.Enabled || err != nil || p.Sign() <= 0 {
			return fmt.Errorf("invalid stored model")
		}
		delete(wanted, key)
		for _, v := range []*json.Number{m.Output, m.CacheRead, m.CacheWrite, m.Cache1h} {
			if v != nil {
				if _, err := number(*v); err != nil {
					return fmt.Errorf("invalid stored price")
				}
			}
		}
	}
	if len(wanted) != 0 {
		return fmt.Errorf("incomplete stored snapshot")
	}
	s.publish(r)
	return nil
}
