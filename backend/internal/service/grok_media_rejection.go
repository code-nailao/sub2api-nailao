package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// LoadGrokVideoRejectionReservation performs an authenticated owner lookup. A
// storage error is not a cache miss: callers must not fall back to another
// account or guess the ownership/price of a charged task.
func (s *OpenAIGatewayService) LoadGrokVideoRejectionReservation(ctx context.Context, taskID string, userID, keyID int64) (*UpstreamRejectionReservation, error) {
	if s == nil {
		return nil, nil
	}
	repo, ok := s.usageBillingRepo.(UpstreamRejectionBillingRepository)
	if !ok {
		return nil, nil
	}
	r, err := repo.GetUpstreamRejectionByTask(ctx, userID, keyID, taskID)
	if errors.Is(err, ErrUpstreamRejectionNotFound) {
		return nil, nil
	}
	return r, err
}

func grokVideoPendingFromReservation(r *UpstreamRejectionReservation) *GrokVideoPendingBilling {
	if r == nil || r.Task == nil {
		return nil
	}
	return &GrokVideoPendingBilling{
		UpstreamRejectionReservationID: r.RequestID,
		VideoBasePricePerSecond:        r.Task.VideoBasePricePerSecond, VideoFinalPricePerSecond: r.Task.VideoFinalPricePerSecond,
		Model: r.Model, BillingModel: r.Model,
		UpstreamModel: r.Task.UpstreamModel, OriginalModel: r.Task.OriginalModel,
		VideoResolution: r.Task.Resolution, VideoDurationSeconds: r.Task.DurationSeconds,
		CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func pendingVideoBasePrice(p *GrokVideoPendingBilling) *float64 {
	if p == nil {
		return nil
	}
	return p.VideoBasePricePerSecond
}

func pendingVideoFinalPrice(p *GrokVideoPendingBilling) *float64 {
	if p == nil {
		return nil
	}
	return p.VideoFinalPricePerSecond
}

// RestoreGrokVideoRejectionBillingKey is an in-memory pricing view of an
// already admitted task, not a key/group mutation or an authorization bypass.
// Caller authentication and task owner checks remain mandatory.
func RestoreGrokVideoRejectionBillingKey(key *APIKey, r *UpstreamRejectionReservation) (*APIKey, error) {
	if key == nil || r == nil || key.User == nil || key.ID != r.APIKeyID || key.UserID != r.UserID || key.User.ID != r.UserID {
		return nil, ErrUpstreamRejectionConflict
	}
	copyKey := *key
	gid := r.GroupID
	copyKey.GroupID = &gid
	copyKey.Group = &Group{ID: gid, Platform: PlatformGrok, SubscriptionType: SubscriptionTypeStandard,
		RateMultiplier: 1, VideoRateIndependent: true, VideoRateMultiplier: 1,
		UpstreamRejectionBillingEnabled: true, UpstreamRejectionPrice: r.Price}
	return &copyKey, nil
}

// SelectGrokVideoRejectionTaskAccount selects the immutable task owner, not a
// new-generation candidate. It must survive loss of Redis sticky state and
// subsequent group/account scheduling edits. Concurrency admission still runs
// through the handler's normal slot acquisition using this wait plan.
func (s *OpenAIGatewayService) SelectGrokVideoRejectionTaskAccount(ctx context.Context, r *UpstreamRejectionReservation) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	decision := OpenAIAccountScheduleDecision{Layer: openAIAccountScheduleLayerSessionSticky, StickySessionHit: true}
	if s == nil || s.accountRepo == nil || r == nil || r.Task == nil || r.AccountID <= 0 {
		return nil, decision, ErrUpstreamRejectionInvalid
	}
	account, err := s.accountRepo.GetByID(ctx, r.AccountID)
	if err != nil {
		return nil, decision, err
	}
	if account == nil || account.ID != r.AccountID {
		return nil, decision, ErrNoAvailableAccounts
	}
	if err := s.validateGrokRejectionAccountIdentity(ctx, account, r); err != nil {
		return nil, decision, err
	}
	decision.SelectedAccountID, decision.SelectedAccountType = account.ID, account.Type
	cfg := s.schedulingConfig()
	return &AccountSelectionResult{Account: account, WaitPlan: &AccountWaitPlan{
		AccountID: account.ID, MaxConcurrency: max(1, account.Concurrency),
		Timeout: cfg.StickySessionWaitTimeout, MaxWaiting: cfg.StickySessionMaxWaiting,
	}}, decision, nil
}

func (s *OpenAIGatewayService) validateGrokRejectionAccountIdentity(ctx context.Context, account *Account, r *UpstreamRejectionReservation) error {
	identity, err := UpstreamRejectionIdentityHash(account)
	if err == nil && r != nil && r.UpstreamIdentityHash != "" && identity == r.UpstreamIdentityHash {
		return nil
	}
	if r != nil && (r.State == UpstreamRejectionSent || r.State == UpstreamRejectionUnknown) {
		if repo, ok := s.usageBillingRepo.(UpstreamRejectionBillingRepository); ok {
			persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if _, persistErr := repo.FinalizeUpstreamRejection(persistCtx, &UpstreamRejectionFinalizeCommand{
				RequestID: r.RequestID, APIKeyID: r.APIKeyID, Outcome: UpstreamRejectionUnknown,
			}); persistErr != nil {
				return persistErr
			}
		}
	}
	return ErrUpstreamRejectionConflict
}

func validateGrokRejectionTaskResponse(account *Account, taskID string, body []byte) error {
	if account.GetUpstreamRejectionProfile() == "" || strings.TrimSpace(taskID) == "" {
		return nil
	}
	if echoedID := extractGrokMediaVideoRequestID(body); echoedID != "" && echoedID != strings.TrimSpace(taskID) {
		return fmt.Errorf("grok video task response identity mismatch")
	}
	return nil
}

// grokMediaRejectionResult preserves the billable outcome independently from
// HTTP success and media units. The classifier never guesses a semantic 400 or
// 403 from an HTTP 200 failed task; unknown supplier outcomes remain uncharged.
func grokMediaRejectionResult(account *Account, status int, body []byte, model, requestID string) *OpenAIForwardResult {
	// The output observer intentionally accepts only 2xx assistant text. HTTP
	// rejection errors use a different allowlist of structured error fields.
	var evidence *UpstreamRejectionEvidence
	if status >= 400 {
		evidence = DetectUpstreamRejectionHTTP(account, status, body)
	} else {
		evidence = observeUpstreamRejectionBody(account, status, body)
	}
	if evidence == nil {
		return nil
	}
	usage, _ := extractOpenAIUsageFromJSONBytes(body)
	return &OpenAIForwardResult{
		RequestID:         requestID,
		Model:             model,
		BillingModel:      model,
		Usage:             usage,
		UpstreamRejection: evidence,
	}
}

// ReconcileGrokVideoRejection observes one existing upstream task. It never
// creates a generation, downloads signed media, or relies on the customer
// polling again. The owner supplies the persisted key identity, including the
// create-time group (a later key-group change must not change the old charge).
func (s *OpenAIGatewayService) ReconcileGrokVideoRejection(ctx context.Context, r *UpstreamRejectionReservation, key *APIKey, quotaUpdater APIKeyQuotaUpdater) (bool, error) {
	if r == nil || r.Task == nil || key == nil || key.User == nil || key.Group == nil ||
		key.ID != r.APIKeyID || key.UserID != r.UserID || key.User.ID != r.UserID || key.Group.ID != r.GroupID || r.Task.TaskID == "" {
		return false, ErrUpstreamRejectionInvalid
	}
	if r.State != UpstreamRejectionSent && r.State != UpstreamRejectionUnknown {
		return true, nil
	}
	if s == nil || s.accountRepo == nil || s.httpUpstream == nil {
		return false, fmt.Errorf("video rejection reconciliation dependencies unavailable")
	}
	repo, ok := s.usageBillingRepo.(UpstreamRejectionBillingRepository)
	if !ok {
		return false, fmt.Errorf("durable video rejection billing unavailable")
	}
	account, err := s.accountRepo.GetByID(ctx, r.AccountID)
	if err != nil {
		return false, err
	}
	if account == nil || account.ID != r.AccountID || !IsUpstreamRejectionProfile(r.ProviderProfile) {
		return false, ErrUpstreamRejectionConflict
	}
	if err := s.validateGrokRejectionAccountIdentity(ctx, account, r); err != nil {
		return false, err
	}
	// The policy remains the admitted policy even if the admin later disables it.
	copyAccount := *account
	copyAccount.Extra = make(map[string]any, len(account.Extra)+3)
	for k, v := range account.Extra {
		copyAccount.Extra[k] = v
	}
	copyAccount.Extra["upstream_rejection_profile"] = r.ProviderProfile
	copyAccount.Extra["upstream_rejection_cost"] = r.UpstreamCost
	copyAccount.Extra["upstream_rejection_currency"] = r.UpstreamCurrency
	account = &copyAccount
	credential, _, err := s.getRequestCredential(ctx, nil, account)
	if err != nil {
		return false, err
	}
	target, err := buildGrokMediaURL(account, s.cfg, GrokMediaEndpointVideoStatus, r.Task.TaskID)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(WithHTTPUpstreamRedirectsDisabled(ctx), http.MethodGet, target, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Accept", "application/json")
	account.ApplyHeaderOverrides(req.Header)
	proxy := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	const limit = 2 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(body) > limit {
		return false, fmt.Errorf("video reconciliation response unreadable or too large")
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return false, fmt.Errorf("video reconciliation redirect refused")
	}
	if err := validateGrokRejectionTaskResponse(account, r.Task.TaskID, body); err != nil {
		return false, err
	}
	upstreamID := firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id"))
	result := grokMediaRejectionResult(account, resp.StatusCode, body, r.Model, upstreamID)
	if result == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		result = ExtractGrokVideoBillingFromStatusBody(body, grokVideoPendingFromReservation(r), r.Task.TaskID)
	}
	if result == nil {
		state := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "status").String()))
		if state == "failed" || state == "expired" {
			_, err = repo.FinalizeUpstreamRejection(ctx, &UpstreamRejectionFinalizeCommand{
				RequestID: r.RequestID, APIKeyID: r.APIKeyID, Outcome: UpstreamRejectionUnknown,
				Evidence: UpstreamRejectionBillingEvidence{OriginalStatus: resp.StatusCode, UpstreamRequestID: storableUpstreamRequestID(upstreamID), ResponseDigest: HashUsageRequestPayload(body)},
			})
			return false, err
		}
		if resp.StatusCode >= 400 {
			return false, fmt.Errorf("video reconciliation upstream status %d", resp.StatusCode)
		}
		return false, nil
	}
	result.RequestID = r.RequestID
	result.ResponseID = r.Task.TaskID
	result.UpstreamRejectionReservationID = r.RequestID
	result.UpstreamModel = r.Task.UpstreamModel
	result.UpstreamHeaders = resp.Header.Clone()
	result.ResponseHeaders = resp.Header.Clone()
	result.Duration = time.Since(r.CreatedAt)
	err = s.RecordUsage(ctx, &OpenAIRecordUsageInput{
		Result: result, APIKey: key, User: key.User, Account: account,
		RequestPayloadHash: r.RequestPayloadHash, APIKeyService: quotaUpdater,
		QuotaPlatform: PlatformGrok, InboundEndpoint: "/v1/videos/generations",
		UpstreamEndpoint: "/v1/videos/{request_id}", PricingAt: r.CreatedAt,
		ChannelUsageFields: ChannelUsageFields{OriginalModel: r.Task.OriginalModel, ChannelMappedModel: r.Model},
	})
	return err == nil, err
}
