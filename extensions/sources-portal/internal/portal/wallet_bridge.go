package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

type WalletBridge struct {
	MainOrigin  string
	Origin      string
	Token       string
	Client      *http.Client
	gate        chan struct{}
	lastSuccess atomic.Int64
}

const walletBridgeResponseLimit = 4096

func NewWalletBridge(origin, token string) *WalletBridge {
	return &WalletBridge{Origin: origin, Token: token, Client: &http.Client{Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 5 * time.Second, IdleConnTimeout: 60 * time.Second}, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, gate: make(chan struct{}, 1)}
}

func (b *WalletBridge) Push(ctx context.Context, store *Store) error {
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case b.gate <- struct{}{}:
	case <-call.Done():
		return ErrTransport
	}
	defer func() { <-b.gate }()
	store.mu.Lock()
	sources := []map[string]any{}
	for _, row := range store.state.Records {
		s := row.Source
		item := map[string]any{"id": s.ID, "name": s.Name, "origin": s.Origin, "account_id": s.AccountID, "revision": s.Revision, "threshold": s.Threshold, "threshold_unit": s.ThresholdUnit, "status": s.Status, "error": s.Error, "last_checked_at": s.LastCheckedAt, "next_check_at": s.NextCheckAt}
		if v := s.LastBalance; v != nil {
			item["last_balance"] = map[string]any{"account_id": v.AccountID, "remaining": v.Remaining, "unit": v.Unit, "checked_at": v.CheckedAt}
		}
		sources = append(sources, item)
	}
	bindings := []map[string]any{}
	for _, v := range store.bindings.Bindings {
		if b.MainOrigin != "" && v.MainOrigin != b.MainOrigin {
			continue
		}
		bindings = append(bindings, map[string]any{"account_id": v.AccountID, "source_id": v.SourceID, "identity_marker": v.Marker, "revision": v.Revision})
	}
	store.mu.Unlock()
	payload, err := json.Marshal(map[string]any{"generated_at": time.Now().UTC(), "sources": sources, "bindings": bindings})
	if err != nil {
		return ErrResponse
	}
	req, err := http.NewRequestWithContext(call, http.MethodPost, strings.TrimSuffix(b.Origin, "/")+"/internal/sources-wallet/snapshot", bytes.NewReader(payload))
	if err != nil {
		return ErrTransport
	}
	req.Header.Set("Authorization", "Bearer "+b.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := b.Client.Do(req)
	if err != nil {
		return ErrTransport
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, walletBridgeResponseLimit))
		return ErrTransport
	}
	mediaType, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return ErrResponse
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, walletBridgeResponseLimit+1))
	if err != nil {
		return ErrTransport
	}
	var receipt struct {
		Code *int `json:"code"`
		Data *struct {
			Synchronized bool `json:"synchronized"`
		} `json:"data"`
	}
	// Main's native envelope is required: an HTTP 200 SPA fallback or proxy
	// page is not evidence that the transactional snapshot was accepted.
	if len(raw) > walletBridgeResponseLimit || json.Unmarshal(raw, &receipt) != nil ||
		receipt.Code == nil || *receipt.Code != 0 || receipt.Data == nil || !receipt.Data.Synchronized {
		return ErrResponse
	}
	b.lastSuccess.Store(time.Now().Unix())
	return nil
}

func (b *WalletBridge) Run(ctx context.Context, store *Store) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		_ = b.Push(ctx, store)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func managedWalletPath(method string, path string) bool {
	return method == "GET" && strings.HasPrefix(path, "/api/v1/admin/accounts/wallet-summaries?") || method == "PUT" && strings.HasPrefix(path, "/api/v1/admin/accounts/wallet-source-binding?")
}
