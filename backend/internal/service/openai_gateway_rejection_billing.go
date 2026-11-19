package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

var ErrUpstreamRejectionBillingUnavailable = errors.New("upstream rejection billing unavailable")
var ErrUpstreamRejectionOutcomeUnknown = errors.New("upstream outcome requires billing reconciliation")

// This is an attempt owned by the gateway. It is never derived from a client ID.
type UpstreamRejectionBillingAttempt struct {
	Reservation              *UpstreamRejectionReservation
	APIKey                   *APIKey
	Account                  *Account
	mu                       sync.Mutex
	Sent                     bool
	SendUncertain            bool
	VideoBasePricePerSecond  *float64
	VideoFinalPricePerSecond *float64
	// trace is the upstream's response to the one request this attempt sent,
	// recorded at the transport seam. Guarded by mu.
	trace *upstreamRejectionTrace
}

func (s *OpenAIGatewayService) BeginUpstreamRejectionBilling(ctx context.Context, key *APIKey, account *Account, model string, payload []byte) (*UpstreamRejectionBillingAttempt, error) {
	if key == nil || key.Group == nil || !key.Group.UpstreamRejectionBillingEnabled {
		return nil, nil
	}
	if s == nil || (s.cfg != nil && s.cfg.RunMode == config.RunModeSimple) || key.User == nil || key.User.ID != key.UserID || key.GroupID == nil ||
		key.Group.ID != *key.GroupID || key.Group.Platform != PlatformGrok || key.Group.SubscriptionType != SubscriptionTypeStandard ||
		account == nil || account.Type != AccountTypeAPIKey || account.GetUpstreamRejectionProfile() == "" ||
		strings.TrimSpace(model) != model || strings.ContainsAny(model, "/*") || len(payload) == 0 {
		return nil, ErrUpstreamRejectionInvalid
	}
	repo, ok := s.usageBillingRepo.(UpstreamRejectionBillingRepository)
	if !ok {
		return nil, ErrUpstreamRejectionBillingUnavailable
	}
	var basePrice, finalPrice *float64
	if strings.HasPrefix(model, "grok-imagine-video") {
		info := ParseGrokMediaRequest("application/json", payload)
		price := key.Group.GetVideoPriceForModel(model, info.Resolution)
		if price == nil || !validNonnegativeRejectionAmount(*price) {
			return nil, ErrUpstreamRejectionInvalid
		}
		if resolved := s.resolveOpenAIChannelPricing(ctx, model, key); resolved != nil && resolved.Source == PricingSourceGroup {
			return nil, errors.New("rejection-enabled video requires explicit group per-second prices")
		}
		base := *price
		final := base * resolveVideoRateMultiplier(key, s.ResolveUserGroupRateMultiplier(ctx, key.UserID, *key.GroupID, key.Group.RateMultiplier))
		final = QuantizeUsageBillingAmount(final)
		if !validNonnegativeRejectionAmount(final) {
			return nil, ErrUpstreamRejectionInvalid
		}
		basePrice, finalPrice = &base, &final
	}
	if !strings.HasPrefix(model, "grok-imagine-video") {
		var options map[string]json.RawMessage
		if err := json.Unmarshal(payload, &options); err != nil {
			return nil, ErrUpstreamRejectionInvalid
		}
		for _, name := range []string{"background", "deferred"} {
			if v, ok := options[name]; ok && string(v) != "false" && string(v) != "null" {
				return nil, errors.New("rejection-enabled text supports synchronous HTTP and SSE only")
			}
		}
	}
	identityHash, identityErr := UpstreamRejectionIdentityHash(account)
	if identityErr != nil {
		return nil, identityErr
	}
	cmd := &UpstreamRejectionReserveCommand{
		RequestID: NewUpstreamRejectionRequestID(), UserID: key.UserID, APIKeyID: key.ID,
		AccountID: account.ID, GroupID: *key.GroupID, Model: model, RequestPayloadHash: HashUsageRequestPayload(payload), UpstreamIdentityHash: identityHash,
		PolicyVersion: fmt.Sprintf("group:%d:%d", key.Group.ID, key.Group.UpdatedAt.UnixNano()),
		RuleVersion:   UpstreamRejectionRuleVersion(), ProviderProfile: account.GetUpstreamRejectionProfile(),
		Price: key.Group.UpstreamRejectionPrice, UpstreamCost: account.GetUpstreamRejectionCost(), UpstreamCurrency: account.GetUpstreamRejectionCurrency(),
	}
	if err := cmd.Validate(); err != nil {
		return nil, err
	}
	reserved, err := repo.ReserveUpstreamRejection(ctx, cmd)
	if err != nil {
		return nil, err
	}
	s.invalidateUpstreamRejectionCaches(ctx, key, nil)
	if reserved == nil || !reserved.Applied || reserved.Reservation == nil {
		return nil, ErrUpstreamRejectionConflict
	}
	return &UpstreamRejectionBillingAttempt{Reservation: reserved.Reservation, APIKey: key, Account: account, VideoBasePricePerSecond: basePrice, VideoFinalPricePerSecond: finalPrice}, nil
}

func (s *OpenAIGatewayService) FinishUpstreamRejectionBillingAttempt(ctx context.Context, attempt *UpstreamRejectionBillingAttempt, result *OpenAIForwardResult, forwardErr error) error {
	if attempt == nil {
		return forwardErr
	}
	if attempt.Reservation == nil || attempt.APIKey == nil {
		return ErrUpstreamRejectionInvalid
	}
	r := attempt.Reservation
	if result != nil {
		result.UpstreamRejectionReservationID = r.RequestID
		result.VideoBasePricePerSecond = attempt.VideoBasePricePerSecond
		result.VideoFinalPricePerSecond = attempt.VideoFinalPricePerSecond
		result.RequestID = r.RequestID
		if result.UpstreamRejection != nil {
			repo, ok := s.usageBillingRepo.(UpstreamRejectionBillingRepository)
			if !ok {
				return ErrUpstreamRejectionBillingUnavailable
			}
			billingCtx, cancel := detachedBillingContext(ctx)
			defer cancel()
			_, err := repo.FinalizeUpstreamRejection(billingCtx, &UpstreamRejectionFinalizeCommand{RequestID: r.RequestID, APIKeyID: r.APIKeyID, Outcome: UpstreamRejectionRejected, Evidence: rejectionBillingEvidence(attempt.Account, result, r.Model)})
			if errors.Is(err, ErrUpstreamRejectionWaived) {
				// Released at its deadline: recorded, not charged, and terminal.
				return forwardErr
			}
			if err != nil {
				return err
			}
			s.invalidateUpstreamRejectionCaches(billingCtx, attempt.APIKey, nil)
			return forwardErr
		}
		pendingVideo := strings.HasPrefix(r.Model, "grok-imagine-video") && result.ResponseID != "" && result.VideoCount == 0
		mediaCompleted := result.ImageCount > 0 || result.VideoCount > 0 || result.AudioUsage != nil
		if forwardErr == nil && (result.UpstreamTerminalEvent != "" || mediaCompleted || pendingVideo) && (hasRejectionNormalUsage(result) || pendingVideo) {
			return nil
		}
		// Partial usage is not a terminal normal bill. The provider may still
		// finish as a fixed-charge rejection after this connection failed.
		if err := s.markUpstreamRejectionUnknownWith(ctx, r, attempt.observedEvidence(result)); err != nil {
			return err
		}
		return ErrUpstreamRejectionOutcomeUnknown
	}
	repo, ok := s.usageBillingRepo.(UpstreamRejectionBillingRepository)
	if !ok {
		return ErrUpstreamRejectionBillingUnavailable
	}
	cmd := &UpstreamRejectionFinalizeCommand{RequestID: r.RequestID, APIKeyID: r.APIKeyID, Outcome: UpstreamRejectionUnknown}
	attempt.mu.Lock()
	sent, uncertain := attempt.Sent, attempt.SendUncertain
	attempt.mu.Unlock()
	if !sent && !uncertain {
		cmd.Outcome, cmd.ConfirmedNoCharge = UpstreamRejectionReleased, true
	}
	var failure *UpstreamFailoverError
	var httpFailure *UpstreamRejectionHTTPError
	status := 0
	if errors.As(forwardErr, &failure) {
		status = failure.StatusCode
	}
	if errors.As(forwardErr, &httpFailure) {
		status = httpFailure.StatusCode
	}
	if sent && status != 0 {
		// Only an observed, fully classified nonbillable HTTP rejection permits
		// another attempt. Transport errors/5xx and missing usage are ambiguous.
		switch status {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusTooManyRequests:
			cmd.Outcome, cmd.ConfirmedNoCharge = UpstreamRejectionReleased, true
		}
	}
	if sent {
		// What the upstream answered, for supplier reconciliation only: neither
		// the status nor an identifier changes the outcome decided above. The
		// stored status is the one the response carried. An error can carry a
		// status that no upstream sent (a transport failure is reported as 502),
		// so the error's status is kept only for a release, which was decided
		// on it, and only when the response itself was not seen.
		cmd.Evidence = attempt.observedEvidence(nil)
		if cmd.Evidence.OriginalStatus == 0 && cmd.Outcome == UpstreamRejectionReleased {
			cmd.Evidence.OriginalStatus = status
		}
	}
	billingCtx, cancel := detachedBillingContext(ctx)
	defer cancel()
	if _, err := repo.FinalizeUpstreamRejection(billingCtx, cmd); err != nil {
		if !errors.Is(err, ErrUpstreamRejectionWaived) {
			return err
		}
		// Already released at its deadline; nothing is left to hold or release.
	} else {
		s.invalidateUpstreamRejectionCaches(billingCtx, attempt.APIKey, nil)
	}
	if cmd.Outcome == UpstreamRejectionUnknown {
		return ErrUpstreamRejectionOutcomeUnknown
	}
	return forwardErr
}

func (s *OpenAIGatewayService) invalidateUpstreamRejectionCaches(ctx context.Context, key *APIKey, updater APIKeyQuotaUpdater) {
	if key == nil {
		return
	}
	cacheCtx, cancel := detachedBillingContext(ctx)
	defer cancel()
	if s.billingCacheService != nil {
		_ = s.billingCacheService.InvalidateUserBalance(cacheCtx, key.UserID)
		_ = s.billingCacheService.InvalidateAPIKeyRateLimit(cacheCtx, key.ID)
	}
	if invalidator, ok := updater.(apiKeyAuthCacheInvalidator); ok && key.Key != "" {
		invalidator.InvalidateAuthCacheByKey(cacheCtx, key.Key)
	}
}

func (s *OpenAIGatewayService) rejectionReservationForUsage(ctx context.Context, in *OpenAIRecordUsageInput) (*UpstreamRejectionReservation, error) {
	if in == nil || in.Result == nil || in.APIKey == nil || in.User == nil || in.Account == nil {
		return nil, ErrUpstreamRejectionInvalid
	}
	id := in.Result.UpstreamRejectionReservationID
	if id == "" {
		return nil, nil
	}
	repo, ok := s.usageBillingRepo.(UpstreamRejectionBillingRepository)
	if !ok {
		return nil, ErrUpstreamRejectionBillingUnavailable
	}
	r, err := repo.GetUpstreamRejection(ctx, id, in.APIKey.ID)
	if err != nil {
		return nil, err
	}
	if r == nil || r.UserID != in.User.ID || in.APIKey.UserID != r.UserID || r.APIKeyID != in.APIKey.ID || r.AccountID != in.Account.ID ||
		in.APIKey.GroupID == nil || r.GroupID != *in.APIKey.GroupID {
		return nil, ErrUpstreamRejectionConflict
	}
	if r.State == UpstreamRejectionRejected {
		return r, nil
	}
	if r.State == UpstreamRejectionUnknown && r.Task == nil && in.Result.UpstreamRejection == nil {
		return nil, ErrUpstreamRejectionOutcomeUnknown
	}
	if in.Result.UpstreamRejection == nil && !hasRejectionNormalUsage(in.Result) {
		return nil, ErrUpstreamRejectionOutcomeUnknown
	}
	if firstNonEmpty(in.OriginalModel, in.Result.Model) != r.Model {
		return nil, ErrUpstreamRejectionConflict
	}
	if r.Task != nil && in.Result.UpstreamRejection == nil {
		if r.Task.VideoBasePricePerSecond == nil || r.Task.VideoFinalPricePerSecond == nil {
			return nil, ErrUpstreamRejectionIntegrity
		}
		in.Result.VideoBasePricePerSecond = r.Task.VideoBasePricePerSecond
		in.Result.VideoFinalPricePerSecond = r.Task.VideoFinalPricePerSecond
	}
	if r.Task != nil {
		in.Result.UpstreamModel = r.Task.UpstreamModel
	}
	in.PricingAt = r.CreatedAt
	in.Result.RequestID = r.RequestID
	in.RequestPayloadHash = r.RequestPayloadHash
	return r, nil
}

// The ledger, not the asynchronously written usage row, is the charge authority.
// A retry repairs a missing usage projection without charging the user again.
func (s *OpenAIGatewayService) recordUpstreamRejectionUsage(ctx context.Context, in *OpenAIRecordUsageInput, r *UpstreamRejectionReservation) error {
	e := in.Result.UpstreamRejection
	if r == nil {
		return ErrUpstreamRejectionConflict
	}
	if r.State == UpstreamRejectionRejected {
		return s.ProjectUpstreamRejectionUsage(ctx, r)
	}
	if e == nil || e.Profile != r.ProviderProfile || e.RuleVersion != r.RuleVersion {
		return ErrUpstreamRejectionConflict
	}
	repo, ok := s.usageBillingRepo.(UpstreamRejectionBillingRepository)
	if !ok {
		return ErrUpstreamRejectionBillingUnavailable
	}
	billingCtx, cancel := detachedBillingContext(ctx)
	defer cancel()
	if r.State != UpstreamRejectionRejected {
		settled, err := repo.FinalizeUpstreamRejection(billingCtx, &UpstreamRejectionFinalizeCommand{RequestID: r.RequestID, APIKeyID: r.APIKeyID, Outcome: UpstreamRejectionRejected, Evidence: rejectionBillingEvidence(in.Account, in.Result, r.Model)})
		if errors.Is(err, ErrUpstreamRejectionWaived) {
			// Released at its deadline: recorded once, never charged or retried.
			return nil
		}
		if err != nil {
			// A concurrent video poll may have committed the same fixed bill
			// with different observational metadata. Repair from its immutable
			// receipt rather than treating the later poll as a new charge.
			committed, readErr := repo.GetUpstreamRejection(billingCtx, r.RequestID, r.APIKeyID)
			if readErr == nil && committed != nil && committed.State == UpstreamRejectionRejected && committed.UserID == r.UserID && committed.AccountID == r.AccountID && committed.RequestPayloadHash == r.RequestPayloadHash {
				s.invalidateUpstreamRejectionCaches(billingCtx, in.APIKey, in.APIKeyService)
				return s.ProjectUpstreamRejectionUsage(billingCtx, committed)
			}
			return err
		}
		if settled == nil || settled.Reservation == nil {
			return ErrUpstreamRejectionIntegrity
		}
		r = settled.Reservation
	}
	s.invalidateUpstreamRejectionCaches(billingCtx, in.APIKey, in.APIKeyService)
	return s.ProjectUpstreamRejectionUsage(billingCtx, r)
}

// ProjectUpstreamRejectionUsage repairs only the display projection. Its sole
// authority is a previously committed PG rejection, never a new poll body.
func (s *OpenAIGatewayService) ProjectUpstreamRejectionUsage(ctx context.Context, r *UpstreamRejectionReservation) error {
	if s == nil || r == nil || r.State != UpstreamRejectionRejected {
		return ErrUpstreamRejectionState
	}
	e := r.Evidence
	upstreamID := optionalTrimmedStringPtr(e.UpstreamRequestID)
	mode, multiplier := string(BillingModePerRequest), 1.0
	duration := e.DurationMs
	log := &UsageLog{UserID: r.UserID, APIKeyID: r.APIKeyID, AccountID: r.AccountID, GroupID: &r.GroupID,
		RequestID: r.RequestID, UpstreamRequestID: upstreamID, Model: r.Model, RequestedModel: firstNonEmpty(e.OriginalModel, r.Model),
		UpstreamModel: optionalTrimmedStringPtr(e.UpstreamModel), UpstreamResponseModel: optionalTrimmedStringPtr(e.UpstreamResponseModel),
		InboundEndpoint: optionalTrimmedStringPtr(e.InboundEndpoint), UpstreamEndpoint: optionalTrimmedStringPtr(e.UpstreamEndpoint),

		InputTokens: e.InputTokens, OutputTokens: e.OutputTokens, CacheReadTokens: e.CacheReadTokens, CacheCreationTokens: e.CacheWriteTokens,
		TotalCost: r.Price, ActualCost: r.Price, AccountStatsCost: &r.UpstreamCost, RateMultiplier: 1, AccountRateMultiplier: &multiplier,
		BillingType: BillingTypeBalance, BillingMode: &mode, DurationMs: &duration, Stream: e.Stream,

		CreatedAt: r.UpdatedAt,
		BillingSnapshot: &UsageBillingSnapshot{Version: 1, ChargeKind: "upstream_rejection", FixedCharge: r.Price,
			OriginalStatus: e.OriginalStatus, RejectionRuleID: e.RuleID, RejectionRuleVersion: e.RuleVersion,
			GroupMultiplier: 1, EffectiveRateMultiplier: 1, EffectiveUnitPrices: map[string]float64{"request": r.Price}},
	}
	if s.usageLogRepo == nil {
		return errors.New("rejection charge committed but usage projection unavailable")
	}
	_, err := s.usageLogRepo.Create(ctx, log)
	if err != nil {
		return err
	}
	repo, ok := s.usageBillingRepo.(UpstreamRejectionBillingRepository)
	if !ok {
		return ErrUpstreamRejectionBillingUnavailable
	}
	if err = repo.MarkUpstreamRejectionUsageProjected(ctx, r.RequestID, r.APIKeyID); err != nil {
		return err
	}
	if s.deferredService != nil {
		s.deferredService.ScheduleLastUsedUpdate(r.AccountID)
	}
	return err
}

type upstreamRejectionAttemptContextKey struct{}

func WithUpstreamRejectionBillingAttempt(ctx context.Context, attempt *UpstreamRejectionBillingAttempt) context.Context {
	return context.WithValue(ctx, upstreamRejectionAttemptContextKey{}, attempt)
}

// Mark once at the actual outgoing HTTP transport, after local validation.
func (s *OpenAIGatewayService) MarkUpstreamRejectionBillingSent(ctx context.Context) error {
	attempt, _ := ctx.Value(upstreamRejectionAttemptContextKey{}).(*UpstreamRejectionBillingAttempt)
	if attempt == nil {
		return nil
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.Sent || attempt.SendUncertain {
		return ErrUpstreamRejectionConflict
	}
	if attempt.Reservation == nil || attempt.APIKey == nil {
		return ErrUpstreamRejectionInvalid
	}
	repo, ok := s.usageBillingRepo.(UpstreamRejectionBillingRepository)
	if !ok {
		return ErrUpstreamRejectionBillingUnavailable
	}
	r := attempt.Reservation
	marked, err := repo.MarkUpstreamRejectionSent(ctx, r.RequestID, r.APIKeyID)
	if err != nil {
		// HTTP has not been called even if the PG acknowledgement is lost.
		// Release via a fresh context; failed compensation stays durable for
		// recovery rather than authorizing a request with uncertain funding.
		attempt.SendUncertain = true
		releaseCtx, cancel := detachedBillingContext(ctx)
		defer cancel()
		released, releaseErr := repo.FinalizeUpstreamRejection(releaseCtx, &UpstreamRejectionFinalizeCommand{RequestID: r.RequestID, APIKeyID: r.APIKeyID, Outcome: UpstreamRejectionReleased, ConfirmedNoCharge: true})
		if releaseErr == nil && released != nil {
			attempt.SendUncertain = false
			s.invalidateUpstreamRejectionCaches(releaseCtx, attempt.APIKey, nil)
		}
		return err
	}
	if marked == nil || !marked.Applied || marked.Reservation == nil {
		attempt.SendUncertain = true
		return ErrUpstreamRejectionConflict
	}
	attempt.Reservation = marked.Reservation
	attempt.Sent = true
	return nil
}

func hasRejectionNormalUsage(r *OpenAIForwardResult) bool {
	if r == nil {
		return false
	}
	return r.Usage.InputTokens > 0 || r.Usage.OutputTokens > 0 || r.Usage.CacheReadInputTokens > 0 || r.Usage.CacheCreationInputTokens > 0 || r.ImageCount > 0 || r.VideoCount > 0 || r.AudioUsage != nil || r.SearchCount > 0 || r.WebSearchCalls > 0
}

func validNonnegativeRejectionAmount(v float64) bool {
	return v >= 0 && v <= 1000000 && !math.IsNaN(v) && !math.IsInf(v, 0) && QuantizeUsageBillingAmount(v) == v
}

func rejectionBillingEvidence(account *Account, result *OpenAIForwardResult, originalModel string) UpstreamRejectionBillingEvidence {
	e, u := result.UpstreamRejection, result.Usage
	out := UpstreamRejectionBillingEvidence{RuleID: e.RuleID, RuleVersion: e.RuleVersion, OriginalStatus: e.HTTPStatus, ResponseDigest: e.ResponseHash, InputTokens: max(0, u.InputTokens-u.CacheReadInputTokens-u.CacheCreationInputTokens), OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadInputTokens, CacheWriteTokens: u.CacheCreationInputTokens, OriginalModel: originalModel, UpstreamModel: result.UpstreamModel, UpstreamResponseModel: result.UpstreamResponseModel, UpstreamEndpoint: result.UpstreamEndpoint, Stream: result.Stream, DurationMs: int(result.Duration.Milliseconds())}
	if upstreamID := usageUpstreamRequestIDPtr(account, result.UpstreamHeaders, result.OpenAIWSMode); upstreamID != nil {
		// An id that storage would refuse is dropped: losing it must not lose the charge.
		out.UpstreamRequestID = storableUpstreamRequestID(*upstreamID)
	}
	if !result.OpenAIWSMode {
		// Read from the result alone, so the attempt and the usage task build
		// byte-identical evidence for the same rejection.
		trace := upstreamRejectionTraceFromHeaders(nil, result.UpstreamHeaders)
		out.XClientRequestID, out.XRequestID, out.CFRay = trace.XClientRequestID, trace.XRequestID, trace.CFRay
	}
	return out
}

func (s *OpenAIGatewayService) markUpstreamRejectionUnknown(ctx context.Context, r *UpstreamRejectionReservation) error {
	return s.markUpstreamRejectionUnknownWith(ctx, r, UpstreamRejectionBillingEvidence{})
}

// markUpstreamRejectionUnknownWith keeps what was observed of the upstream's
// response with the unknown outcome. The repository retains the first evidence.
func (s *OpenAIGatewayService) markUpstreamRejectionUnknownWith(ctx context.Context, r *UpstreamRejectionReservation, observed UpstreamRejectionBillingEvidence) error {
	repo, ok := s.usageBillingRepo.(UpstreamRejectionBillingRepository)
	if !ok {
		return ErrUpstreamRejectionBillingUnavailable
	}
	billingCtx, cancel := detachedBillingContext(ctx)
	defer cancel()
	_, err := repo.FinalizeUpstreamRejection(billingCtx, &UpstreamRejectionFinalizeCommand{RequestID: r.RequestID, APIKeyID: r.APIKeyID, Outcome: UpstreamRejectionUnknown, Evidence: observed})
	if errors.Is(err, ErrUpstreamRejectionWaived) {
		// Already released at its deadline; there is no hold left to retain.
		return nil
	}
	return err
}
