package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/processrole"
)

// StartSourcesSettingsRefresh keeps serving-local caches in step with changes
// made through the unique admin/background instance. Read values stay local;
// neither settings nor their hashes are exposed by the runtime control API.
// This also works while the initial background owner is an older native Main.
func (s *SettingService) StartSourcesSettingsRefresh() func() {
	if s == nil || processrole.BackgroundEnabled() {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		var previous [sha256.Size]byte
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			probe, finish := context.WithTimeout(ctx, 4*time.Second)
			values, err := s.settingRepo.GetAll(probe)
			if err == nil {
				raw, _ := json.Marshal(values) // JSON sorts map keys deterministically.
				fingerprint := sha256.Sum256(raw)
				if fingerprint != previous {
					s.refreshSourcesSettings(probe, values)
					previous = fingerprint
				}
			}
			finish()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (s *SettingService) refreshSourcesSettings(ctx context.Context, values map[string]string) {
	// Reuse native cache parsing and invalidation; never call UpdateSettings here.
	s.refreshCachedSettings(s.parseSettings(values))
	cfg := parseWebSearchConfigJSON(values[SettingKeyWebSearchEmulationConfig])
	webSearchEmulationSF.Forget(sfKeyWebSearchConfig)
	webSearchEmulationCache.Store(&cachedWebSearchEmulationConfig{config: cfg, expiresAt: time.Now().Add(webSearchEmulationCacheTTL).UnixNano()})
	if s.webSearchManagerBuilder != nil {
		s.webSearchManagerBuilder(cfg, s.resolveProviderProxyURLs(ctx, cfg))
	}
}
