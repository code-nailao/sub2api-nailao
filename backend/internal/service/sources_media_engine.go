package service

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/processrole"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
	"sources.local/mediaengine"
)

// SourcesImageEngineConfig is an external registry shared with Portal. It is not
// a Main database field and cannot change the engine of already accepted tasks.
type SourcesImageEngineConfig struct {
	GroupID    int64    `json:"group_id"`
	Engine     string   `json:"engine"`
	Models     []string `json:"models"`
	AccountIDs []int64  `json:"account_ids,omitempty"`
}
type SourcesMediaEngine struct {
	creates           singleflight.Group
	Engine            *mediaengine.Engine
	producer          *mediaengine.Producer
	admissionReadOnly bool
	admission         *processrole.AdmissionGate
	legacyAdmission   *sourcesLegacyAdmission
	store             *mediaengine.Store
	gateway           *OpenAIGatewayService
	gemini            *GatewayService
	images            *ImageTaskService
	keys              *APIKeyService
	routes            map[int64]SourcesImageEngineConfig
	ns                mediaengine.Namespace
	downloadClient    *http.Client
}
type SourcesImageBillingError struct{ Cause error }

func (e *SourcesImageBillingError) Error() string { return e.Cause.Error() }
func (e *SourcesImageBillingError) Unwrap() error { return e.Cause }

type sourcesMediaEngineTicket struct {
	Platform  string
	Inputs    []sourcesImageObject
	Version   int
	Owner     mediaengine.Owner
	AccountID int64
	Amount    float64
	Billing   UsageBillingCommand
	Log       UsageLog
}

func NewSourcesMediaEngine(g *OpenAIGatewayService, gemini *GatewayService, images *ImageTaskService, keys *APIKeyService) (*SourcesMediaEngine, error) {
	path := os.Getenv("SOURCES_IMAGE_ENGINES_FILE")
	if path == "" {
		return nil, nil
	}
	// #nosec G703 -- Operator-owned deployment registry from the process environment, never a request path.
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read external image-engine registry: %w", err)
	}
	var entries []SourcesImageEngineConfig
	if json.Unmarshal(raw, &entries) != nil {
		return nil, errors.New("invalid image-engine registry")
	}
	// A registry or store written under a legacy name is refused below unless
	// that name is configured, so a missed setting fails start-up loudly.
	ns, err := sourcesMediaEngineNamespace()
	if err != nil {
		return nil, err
	}
	routes := map[int64]SourcesImageEngineConfig{}
	for _, entry := range entries {
		if entry.GroupID <= 0 || routes[entry.GroupID].GroupID != 0 {
			return nil, errors.New("invalid or duplicate image group")
		}
		if entry.Engine != "sub2api" && !ns.IsSelector(entry.Engine) {
			return nil, errors.New("unsupported image engine")
		}
		if ns.IsSelector(entry.Engine) {
			// Either accepted spelling selects the engine; one canonical value
			// keeps every later comparison a plain equality.
			entry.Engine = ns.Selector()
			if len(entry.AccountIDs) == 0 || len(entry.Models) == 0 {
				return nil, errors.New("media engine groups need explicit accounts and models")
			}
			for _, model := range entry.Models {
				if !sourcesMediaEngineModels[model] {
					return nil, errors.New("unsupported media engine model")
				}
			}
		}
		routes[entry.GroupID] = entry
	}
	keyFile := os.Getenv("SOURCES_MEDIA_ENGINE_KEY_FILE")
	dir := os.Getenv("SOURCES_MEDIA_ENGINE_DATA_DIR")
	// #nosec G703 -- Fixed operator-mounted secret path; owner-only mode is checked below.
	info, err := os.Stat(keyFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || dir == "" {
		return nil, errors.New("owner-only media engine key and independent data directory required")
	}
	// #nosec G703 -- Same operator-mounted, owner-only secret checked above; no request input.
	encoded, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil || len(key) != 32 {
		return nil, errors.New("invalid media engine encryption key")
	}
	if g == nil || g.usageBillingRepo == nil || g.usageLogRepo == nil || g.accountRepo == nil || g.billingService == nil || g.billingCacheService == nil || g.concurrencyService == nil || keys == nil {
		return nil, errors.New("native media engine boundaries unavailable")
	}
	if g.cfg != nil && g.cfg.RunMode != config.RunModeStandard {
		return nil, errors.New("media engine requires standard balance billing mode")
	}
	if _, ok := g.usageBillingRepo.(SourcesImageLedger); !ok {
		return nil, errors.New("atomic native image ledger is required")
	}
	mode := os.Getenv("SOURCES_MEDIA_ENGINE_ADMISSION_MODE")
	if mode != "" && mode != "enabled" && mode != "read-only" {
		return nil, errors.New("invalid media engine admission mode")
	}
	recoveryPolicy, err := loadSourcesMediaEngineRecoveryPolicy(os.Getenv("SOURCES_MEDIA_ENGINE_RECOVERY_POLICY_FILE"))
	if err != nil {
		return nil, err
	}
	recoveryEnablePath := os.Getenv("SOURCES_MEDIA_ENGINE_RECOVERY_ENABLE_FILE")
	openStore := mediaengine.Open
	if !processrole.BackgroundEnabled() {
		openStore = mediaengine.OpenProducer
	}
	store, err := openStore(dir, key, ns)
	if err != nil {
		return nil, err
	}
	download, err := newSourcesImageDownloadClient(g.cfg)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	s := &SourcesMediaEngine{store: store, gateway: g, gemini: gemini, images: images, keys: keys, routes: routes, ns: ns, downloadClient: download, admissionReadOnly: mode == "read-only"}
	opts := mediaengine.Options{RecoveryPolicy: recoveryPolicy, RecoveryEnabled: func() bool {
		return recoveryPolicy != nil && sourcesMediaEngineRecoveryEnabled(recoveryEnablePath, recoveryPolicy.ID)
	}, OnError: func(id, phase string) {
		logger.L().Warn("sources.media_engine.step_pending", zap.String("task_id", id), zap.String("phase", phase))
	}}
	if processrole.BackgroundEnabled() {
		s.Engine, err = mediaengine.NewWithOptions(store, s, sourcesNativeImageProvider{owner: s}, opts)
		if err == nil {
			s.producer = s.Engine.Producer
		}
	} else {
		s.producer, err = mediaengine.NewProducer(store, opts)
	}
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := s.configureAdmission(); err != nil {
		_ = store.Close()
		return nil, err
	}
	processrole.StartBackground("SourcesMediaEngine", func() { s.Engine.Start() })
	return s, nil
}

func (s *SourcesMediaEngine) taskProducer() *mediaengine.Producer {
	if s.producer != nil {
		return s.producer
	}
	if s.Engine != nil {
		return s.Engine.Producer
	}
	return nil
}

func (s *SourcesMediaEngine) GetTask(owner mediaengine.Owner, id string) (*mediaengine.Task, error) {
	if s == nil || s.taskProducer() == nil {
		return nil, mediaengine.ErrNotFound
	}
	return s.taskProducer().Get(owner, id)
}

func (s *SourcesMediaEngine) ReviewPage(phase, after string) ([]*mediaengine.Task, error) {
	if s == nil || s.taskProducer() == nil {
		return nil, mediaengine.ErrNotFound
	}
	return s.taskProducer().ReviewPage(phase, after)
}
func (s *SourcesMediaEngine) IsGroup(id int64) bool {
	return s != nil && s.routes[id].Engine == s.ns.Selector()
}
func (s *SourcesMediaEngine) Submit(ctx context.Context, key *APIKey, endpoint, contentType string, body []byte, idem, ip, ua string, admission *processrole.AdmissionLease) (*mediaengine.Task, error) {
	if key == nil || key.User == nil || key.GroupID == nil {
		return nil, mediaengine.ErrDenied
	}
	if admission == nil || admission.Mode == processrole.AdmissionLegacy {
		return nil, mediaengine.ErrDenied
	}
	parsed, err := parseSourcesMediaEngineRequest(endpoint, contentType, body)
	if err != nil {
		return nil, fmt.Errorf("invalid media engine request: %w", err)
	}
	fingerprint := parsed.Hash
	index := fmt.Sprintf("%d:%d:%d:%s", key.UserID, key.ID, *key.GroupID, idem)
	// Wait for the shared create even when this HTTP caller is cancelled. The
	// admission lease and native slots must outlive staging/persistence; otherwise
	// an operator could release a held batch before its final Create commits.
	value, err, _ := s.creates.Do(index, func() (any, error) {
		requestCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		if stored, err := s.store.Existing(mediaengine.Owner{UserID: key.UserID, KeyID: key.ID, GroupID: *key.GroupID}, idem); err == nil {
			if stored.PayloadHash != fingerprint {
				return nil, mediaengine.ErrConflict
			}
			return stored, nil
		} else if !errors.Is(err, mediaengine.ErrNotFound) {
			return nil, err
		}
		// First migration from 028 keeps media admissions on the legacy owner
		// until its HTTP admissions have drained. Existing tasks stay readable.
		if s.admissionReadOnly {
			return nil, ErrImageTaskUnavailable
		}
		hold := ""
		if admission.Mode == processrole.AdmissionHeld {
			hold = admission.HoldToken
		}
		return s.submitOnce(requestCtx, key, endpoint, contentType, body, idem, ip, ua, hold)
	})
	if err != nil {
		return nil, err
	}
	task, ok := value.(*mediaengine.Task)
	if !ok || task.PayloadHash != fingerprint {
		return nil, mediaengine.ErrConflict
	}
	return task, nil
}

func (s *SourcesMediaEngine) submitOnce(ctx context.Context, key *APIKey, endpoint, contentType string, body []byte, idem, ip, ua, hold string) (*mediaengine.Task, error) {
	if key == nil || key.User == nil || key.Group == nil || key.GroupID == nil || !s.IsGroup(*key.GroupID) || !GroupAllowsImageGeneration(key.Group) || key.Group.IsSubscriptionType() {
		return nil, mediaengine.ErrDenied
	}
	req, err := parseSourcesMediaEngineRequest(endpoint, contentType, body)
	if err != nil {
		return nil, fmt.Errorf("invalid media engine request: %w", err)
	}
	owner := mediaengine.Owner{UserID: key.UserID, KeyID: key.ID, GroupID: *key.GroupID}
	if t, err := s.taskProducer().Existing(owner, idem, req.Hash); err == nil {
		return t, nil
	} else if !errors.Is(err, mediaengine.ErrNotFound) {
		return nil, err
	}
	route := s.routes[*key.GroupID]
	allowed := false
	for _, model := range route.Models {
		if model == req.Model {
			allowed = true
		}
	}
	if !allowed {
		return nil, mediaengine.ErrDenied
	}
	if _, ok := s.images.current(); !ok {
		return nil, errors.New("native image object storage is not configured")
	}
	if err = s.gateway.billingCacheService.CheckBillingEligibility(ctx, key.User, key, key.Group, nil, QuotaPlatform(ctx, key)); err != nil {
		return nil, &SourcesImageBillingError{Cause: err}
	}
	mapping, restricted := s.gateway.ResolveChannelMappingAndRestrict(ctx, key.GroupID, req.Model)
	if restricted {
		return nil, mediaengine.ErrDenied
	}
	routing := req.Model
	if mapping.MappedModel != "" {
		routing = mapping.MappedModel
	}
	account, err := s.selectAccount(ctx, key, req.Model, route)
	if err != nil {
		return nil, err
	}
	credential := mediaengine.Credential{BaseURL: account.GetCredential("base_url"), APIKey: account.GetCredential("api_key"), AccountID: account.ID, Concurrency: account.Concurrency}
	if account.Proxy != nil {
		credential.ProxyURL = account.Proxy.URL()
	}
	if err := mediaengine.ValidateCredential(credential); err != nil {
		return nil, err
	}
	ticket, err := s.quote(ctx, key, account, req, endpoint, ip, ua)
	if err != nil {
		return nil, err
	}
	refs, objects, err := s.stageInputs(ctx, req, fmt.Sprintf("%d-%d-%s", key.UserID, key.ID, HashUsageRequestPayload([]byte(idem))))
	if err != nil {
		return nil, err
	}
	ticket.Inputs = objects
	mapped := account.GetMappedModel(routing)
	payload, err := req.body(mapped, refs)
	if err != nil {
		return nil, err
	}
	ticket.Log.UpstreamModel = optionalTrimmedStringPtr(mapped)
	ticket.Billing.RequestPayloadHash = req.Hash
	encoded, err := json.Marshal(ticket)
	if err != nil {
		return nil, err
	}
	in := mediaengine.Input{Owner: owner, IdempotencyKey: idem, PayloadHash: req.Hash, Body: payload, Ticket: encoded, Credential: credential}
	if hold != "" {
		return s.taskProducer().CreateHeld(in, hold)
	}
	return s.taskProducer().Create(in)
}
func (s *SourcesMediaEngine) quote(ctx context.Context, key *APIKey, a *Account, r *sourcesMediaEngineRequest, endpoint, ip, ua string) (*sourcesMediaEngineTicket, error) {
	group := key.Group
	rate := group.RateMultiplier
	if resolver := s.gateway.userGroupRateResolver; resolver != nil && resolver.repo != nil {
		override, err := resolver.repo.GetByUserAndGroup(ctx, key.UserID, group.ID)
		if err != nil {
			return nil, err
		}
		if override != nil {
			rate = *override
		}
	}
	if group.ImageRateIndependent {
		rate = group.ImageRateMultiplier
	}
	priceModel := r.Model
	// Preserve native explicit model/group/channel priority. Legacy media engine aliases
	// are a compatibility fallback, never a replacement for a public model price.
	freshKey := s.gateway.apiKeyWithFreshGroupMediaPricing(ctx, key)
	explicit := s.gateway.resolveOpenAIChannelPricing(ctx, priceModel, freshKey)
	legacy := sourcesMediaEnginePricingModel(s.ns, r.Model, r.Quality)
	if explicit == nil && !apiKeyHasConfiguredImagePrice(freshKey, r.Size) && legacy != r.Model && s.gateway.resolveOpenAIChannelPricing(ctx, legacy, freshKey) != nil {
		priceModel = legacy
	}
	cost := s.gateway.calculateOpenAIImageCost(ctx, priceModel, freshKey, &OpenAIForwardResult{ImageCount: 1, ImageSize: r.Size}, rate)
	if explicit == nil && priceModel == r.Model && !apiKeyHasConfiguredImagePrice(freshKey, r.Size) && isSourcesMediaEnginePriceModel(s.ns, legacy) {
		// Retained media engine per-image contract, only when no native configured price applies.
		unit := .03
		if strings.HasSuffix(legacy, "/high") {
			unit = .05
		}
		if r.Size == "1K" {
			unit *= .5
		}
		cost = &CostBreakdown{TotalCost: unit, ActualCost: unit * rate, BillingMode: string(BillingModeImage)}
	}
	unit, mode := cost.TotalCost, cost.BillingMode
	if math.IsNaN(unit) || math.IsInf(unit, 0) || unit < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
		return nil, errors.New("invalid immutable image price")
	}
	amount := QuantizeUsageBillingAmount(unit * rate)
	cost.ActualCost = amount
	accountRate := a.BillingRateMultiplier()
	media := "image"
	log := UsageLog{UserID: key.UserID, APIKeyID: key.ID, AccountID: a.ID, Model: r.Model, RequestedModel: r.Model, GroupID: key.GroupID, ImageCount: 1, ImageSize: &r.Size, ImageOutputSize: &r.Size, MediaType: &media, BillingMode: &mode, BillingType: BillingTypeBalance, RequestType: RequestTypeSync, TotalCost: unit, ActualCost: amount, RateMultiplier: rate, AccountRateMultiplier: &accountRate, CreatedAt: time.Now(), InboundEndpoint: optionalTrimmedStringPtr(endpoint), UpstreamEndpoint: optionalTrimmedStringPtr("/v1/videos"), IPAddress: optionalTrimmedStringPtr(ip), UserAgent: optionalTrimmedStringPtr(ua)}
	cmd := buildUsageBillingCommand("", &log, &postUsageBillingParams{Cost: cost, User: key.User, APIKey: key, Account: a, APIKeyService: s.keys, AccountRateMultiplier: accountRate, RequestPayloadHash: r.Hash})
	if cmd == nil {
		return nil, errors.New("native image billing command unavailable")
	}
	cmd.BalanceCost = 0
	cmd.RequestFingerprint = ""
	cmd.Normalize()
	return &sourcesMediaEngineTicket{Platform: QuotaPlatform(ctx, key), Version: 1, Owner: mediaengine.Owner{UserID: key.UserID, KeyID: key.ID, GroupID: *key.GroupID}, AccountID: a.ID, Amount: amount, Billing: *cmd, Log: log}, nil
}
func sourcesImageTicket(t *mediaengine.Task) (*sourcesMediaEngineTicket, error) {
	var v sourcesMediaEngineTicket
	if json.Unmarshal(t.Ticket, &v) != nil || v.Version != 1 || v.Owner != t.Owner || v.AccountID != t.AccountID || v.Billing.UserID != t.Owner.UserID || v.Billing.APIKeyID != t.Owner.KeyID || v.Billing.AccountID != t.AccountID || v.Amount < 0 || math.IsInf(v.Amount, 0) || math.IsNaN(v.Amount) || v.Billing.BalanceCost != 0 || v.Billing.SubscriptionCost != 0 || v.Billing.SubscriptionID != nil || v.Log.UserID != t.Owner.UserID || v.Log.APIKeyID != t.Owner.KeyID || v.Log.AccountID != t.AccountID || v.Log.GroupID == nil || *v.Log.GroupID != t.Owner.GroupID || v.Log.ActualCost != v.Amount || v.Billing.RequestPayloadHash != t.PayloadHash {
		return nil, mediaengine.ErrConflict
	}

	b := v.Billing
	if (v.Platform != PlatformOpenAI && v.Platform != PlatformGemini) || b.AccountType != AccountTypeAPIKey || !sourcesMediaEngineModels[b.Model] || b.Model != v.Log.Model || b.BillingType != BillingTypeBalance || v.Log.BillingType != BillingTypeBalance || b.ImageCount != 1 || v.Log.ImageCount != 1 || b.InputTokens != 0 || b.OutputTokens != 0 || b.CacheCreationTokens != 0 || b.CacheReadTokens != 0 {
		return nil, mediaengine.ErrConflict
	}
	if v.Log.TotalCost < 0 || math.IsNaN(v.Log.TotalCost) || math.IsInf(v.Log.TotalCost, 0) || v.Log.RateMultiplier < 0 || math.IsNaN(v.Log.RateMultiplier) || math.IsInf(v.Log.RateMultiplier, 0) || QuantizeUsageBillingAmount(v.Log.TotalCost*v.Log.RateMultiplier) != v.Amount {
		return nil, mediaengine.ErrConflict
	}
	for _, cost := range []float64{b.APIKeyQuotaCost, b.APIKeyRateLimitCost, b.AccountQuotaCost} {
		if cost < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
			return nil, mediaengine.ErrConflict
		}
	}
	if (b.APIKeyQuotaCost != 0 && b.APIKeyQuotaCost != v.Amount) || (b.APIKeyRateLimitCost != 0 && b.APIKeyRateLimitCost != v.Amount) {
		return nil, mediaengine.ErrConflict
	}
	expected := b
	expected.RequestFingerprint = ""
	expected.Normalize()
	if expected.RequestFingerprint != b.RequestFingerprint {
		return nil, mediaengine.ErrConflict
	}
	return &v, nil
}
func sourcesImageHold(t *mediaengine.Task, v *sourcesMediaEngineTicket, phase string) *BatchImageBalanceHoldCommand {
	id := BatchImageHoldRequestID(t.ID)
	actual := 0.0
	switch phase {
	case "capture":
		id = BatchImageCaptureRequestID(t.ID)
		actual = v.Amount
	case "release":
		id = BatchImageReleaseRequestID(t.ID)
	}
	return &BatchImageBalanceHoldCommand{RequestID: id, APIKeyID: t.Owner.KeyID, UserID: t.Owner.UserID, BatchID: t.ID, HoldAmount: v.Amount, ActualAmount: actual, RequestPayloadHash: t.PayloadHash}
}
func (s *SourcesMediaEngine) invalidate(ctx context.Context, t *mediaengine.Task) error {
	if s.keys != nil {
		s.keys.InvalidateAuthCacheByUserID(ctx, t.Owner.UserID)
	}
	if s.gateway.billingCacheService != nil {
		return errors.Join(s.gateway.billingCacheService.InvalidateUserBalance(ctx, t.Owner.UserID), s.gateway.billingCacheService.InvalidateAPIKeyRateLimit(ctx, t.Owner.KeyID))
	}
	return nil
}
func (s *SourcesMediaEngine) Reserve(ctx context.Context, t *mediaengine.Task) error {
	v, err := sourcesImageTicket(t)
	if err != nil {
		return err
	}
	_, err = s.gateway.usageBillingRepo.ReserveBatchImageBalance(ctx, sourcesImageHold(t, v, "hold"))
	if errors.Is(err, ErrBatchImageInsufficientBalance) || errors.Is(err, ErrUserNotFound) {
		return mediaengine.ErrDenied
	}
	if err != nil {
		return err
	}
	return s.invalidate(ctx, t)
}
func (s *SourcesMediaEngine) Release(ctx context.Context, t *mediaengine.Task) error {
	v, err := sourcesImageTicket(t)
	if err != nil {
		if errors.Is(err, mediaengine.ErrConflict) {
			return mediaengine.ErrFinancialConflict
		}
		return err
	}
	ledger, ok := s.gateway.usageBillingRepo.(SourcesImageLedger)
	if !ok {
		return errors.New("atomic native image ledger unavailable")
	}
	err = ledger.ReleaseSourcesImage(ctx, sourcesImageHold(t, v, "release"))
	if err != nil {
		if errors.Is(err, ErrUsageBillingRequestConflict) || errors.Is(err, sql.ErrNoRows) {
			return mediaengine.ErrFinancialConflict
		}
		return err
	}
	return s.invalidate(ctx, t)
}
func (s *SourcesMediaEngine) Settle(ctx context.Context, t *mediaengine.Task) error {
	v, err := sourcesImageTicket(t)
	if err != nil {
		return err
	}
	cmd := v.Billing
	cmd.RequestID = sourcesMediaEngineBillingRequestID(s.ns, t.ID)
	cmd.Normalize()
	ledger, ok := s.gateway.usageBillingRepo.(SourcesImageLedger)
	if !ok {
		return errors.New("atomic native image ledger unavailable")
	}
	result, err := ledger.SettleSourcesImage(ctx, sourcesImageHold(t, v, "capture"), &cmd)
	if err != nil {
		return err
	}
	if result != nil && result.Applied {
		// Native post-effects own platform quotas and notifications. The balance was
		// already removed by the hold, so only invalidate it, never deduct it again.
		params := &postUsageBillingParams{Cost: &CostBreakdown{TotalCost: v.Log.TotalCost, ActualCost: v.Amount}, User: &User{ID: t.Owner.UserID}, APIKey: &APIKey{ID: t.Owner.KeyID, GroupID: &v.Owner.GroupID}, Account: &Account{ID: t.AccountID, Type: v.Billing.AccountType}, Platform: v.Platform, AccountRateMultiplier: optionalFloatValue(v.Log.AccountRateMultiplier), BalanceAlreadyReserved: true, APIKeyService: s.keys}
		if s.gateway.userRepo != nil {
			if user, e := s.gateway.userRepo.GetByID(ctx, t.Owner.UserID); e == nil && user != nil {
				params.User = user
			}
		}
		if account, e := s.gateway.accountRepo.GetByID(ctx, t.AccountID); e == nil && account != nil {
			params.Account = account
		}
		deps := s.gateway.billingDeps()
		deps.cfg = s.gateway.cfg
		finalizePostUsageBilling(ctx, params, deps, result)
	}

	log := v.Log
	if t.UpstreamID == "" {
		model := log.Model
		if log.UpstreamModel != nil {
			model = *log.UpstreamModel
		}
		log.UpstreamEndpoint = optionalTrimmedStringPtr(sourcesNativeImagePath(v.Platform, model, len(v.Inputs) > 0))
	}
	log.RequestID = cmd.RequestID
	duration := int(time.Since(time.Unix(t.CreatedAt, 0)).Milliseconds())
	log.DurationMs = &duration
	log.UpstreamRequestID = optionalTrimmedStringPtr(t.UpstreamID)
	if _, err = s.gateway.usageLogRepo.Create(ctx, &log); err != nil {
		return err
	}
	if s.gateway.deferredService != nil {
		s.gateway.deferredService.ScheduleLastUsedUpdate(t.AccountID)
	}
	return s.invalidate(ctx, t)
}

// Native account concurrency protects actual HTTP I/O, not queued task count.
func (s *SourcesMediaEngine) Do(req *http.Request, c mediaengine.Credential) (*http.Response, error) {
	slot, err := s.gateway.concurrencyService.AcquireAccountSlot(req.Context(), c.AccountID, c.Concurrency)
	if err != nil || slot == nil || !slot.Acquired {
		return nil, mediaengine.ErrDeferred
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			if slot.ReleaseFunc != nil {
				slot.ReleaseFunc()
			}
		})
	}
	resp, err := s.gateway.httpUpstream.Do(req.WithContext(s.transportContext(req.Context())), c.ProxyURL, c.AccountID, c.Concurrency)
	if err != nil {
		release()
		return nil, err
	}
	resp.Body = &sourcesImageBody{ReadCloser: resp.Body, release: release}
	return resp, nil
}

type sourcesImageBody struct {
	io.ReadCloser
	release func()
}

func (b *sourcesImageBody) Close() error { defer b.release(); return b.ReadCloser.Close() }

func (s *SourcesMediaEngine) Stop() {
	if s != nil {
		if s.Engine != nil {
			s.Engine.CloseGracefully()
		}
		_ = s.store.Close()
		if s.admission != nil {
			_ = s.admission.Close()
		}
	}
}

// Handles also recognizes earlier media engine submissions after a registry change.
// This lookup cannot turn an existing idempotent retry into an ordinary POST.
func (s *SourcesMediaEngine) Handles(key *APIKey, idem string) (bool, error) {
	if s == nil || key == nil || key.GroupID == nil {
		return false, nil
	}
	if s.IsGroup(*key.GroupID) {
		return true, nil
	}
	if idem == "" {
		return false, nil
	}
	_, err := s.store.Existing(mediaengine.Owner{UserID: key.UserID, KeyID: key.ID, GroupID: *key.GroupID}, idem)
	if errors.Is(err, mediaengine.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Only the allowlisted native accounts are eligible. Native schedulers still own
// priority, load, model restrictions and runtime bans. No engine or group fallback.
func (s *SourcesMediaEngine) selectAccount(ctx context.Context, key *APIKey, model string, route SourcesImageEngineConfig) (*Account, error) {
	accounts, err := s.gateway.accountRepo.ListByGroup(ctx, route.GroupID)
	if err != nil {
		return nil, err
	}
	allowed := make(map[int64]bool)
	for _, id := range route.AccountIDs {
		allowed[id] = true
	}
	excluded := make(map[int64]struct{})
	eligible := make(map[int64]bool)
	for _, a := range accounts {
		if allowed[a.ID] && a.Type == AccountTypeAPIKey && a.Platform == key.Group.Platform {
			eligible[a.ID] = true
		} else {
			excluded[a.ID] = struct{}{}
		}
	}
	if len(eligible) == 0 {
		return nil, ErrNoAvailableAccounts
	}
	ctx = WithOpenAIProfitControlSuppressed(WithOpenAIImageGenerationIntent(ctx))
	var selection *AccountSelectionResult
	if key.Group.Platform == PlatformOpenAI {
		selection, _, err = s.gateway.selectAccountWithScheduler(ctx, key.GroupID, "", "", model, excluded, OpenAIUpstreamTransportHTTPSSE, "", OpenAIImagesCapabilityAPIKey, false, PlatformOpenAI, false, false)
	} else if key.Group.Platform == PlatformGemini && s.gemini != nil {
		ctx = context.WithValue(ctx, ctxkey.ForcePlatform, PlatformGemini)
		selection, err = s.gemini.SelectAccountWithLoadAwareness(ctx, key.GroupID, "", model, excluded, "", key.UserID)
	} else {
		return nil, ErrNoAvailableAccounts
	}
	if selection != nil && selection.ReleaseFunc != nil {
		defer selection.ReleaseFunc()
	}
	if err != nil {
		return nil, err
	}
	if selection == nil || selection.Account == nil || !eligible[selection.Account.ID] || !selection.Account.IsSchedulable() {
		return nil, ErrNoAvailableAccounts
	}
	return selection.Account, nil
}
func (s *SourcesMediaEngine) transportContext(ctx context.Context) context.Context {
	ctx = WithHTTPUpstreamRedirectsDisabled(ctx)
	if s.gateway.cfg == nil || !s.gateway.cfg.Security.URLAllowlist.AllowPrivateHosts {
		ctx = WithHTTPUpstreamPublicHostsOnly(ctx)
	}
	return ctx
}

// Lookup never submits to either engine. It recovers a lost 202 response.
func (s *SourcesMediaEngine) Lookup(owner mediaengine.Owner, idem string) (*mediaengine.Task, error) {
	return s.store.Existing(owner, idem)
}

func optionalFloatValue(v *float64) float64 {
	if v != nil {
		return *v
	}
	return 1
}

func SourcesImageReviewSummary(t *mediaengine.Task) (string, float64, error) {
	ticket, err := sourcesImageTicket(t)
	if err != nil {
		return "", 0, err
	}
	return ticket.Log.Model, ticket.Amount, nil
}
