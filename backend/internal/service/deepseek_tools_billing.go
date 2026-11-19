package service

import (
	"context"
	"errors"
	"time"

	"github.com/gin-gonic/gin"
)

const deepSeekRequestTimeout = 10 * time.Minute
const deepSeekBillingInputKey = "sources.deepseek.billing_input"

// Only confirmed usage and calculated amounts are persisted. No API keys,
// conversation text, search queries or fetched documents enter this journal.
type DeepSeekUsageCheckpoint struct {
	Command  *UsageBillingCommand
	Usage    *UsageLog
	Revision int
	Ready    bool
}

type DeepSeekUsageIdentity struct {
	RequestID string
	APIKeyID  int64
}

type DeepSeekUsageSettlement struct {
	Command *UsageBillingCommand
	Usage   *UsageLog
	Result  *UsageBillingApplyResult
}

type DeepSeekUsageJournal interface {
	BeginDeepSeekUsage(context.Context, string, int64, int64, int64, time.Time) error
	SaveDeepSeekUsage(context.Context, *DeepSeekUsageCheckpoint) error
	SettleDeepSeekUsage(context.Context, string, int64) (*DeepSeekUsageSettlement, error)
	ListPendingDeepSeekUsage(context.Context, int) ([]DeepSeekUsageIdentity, error)
}

// The state belongs to one admitted request. The journal stores a cumulative
// checkpoint after each observed outcome; recovery only settles that checkpoint,
// and never sends another model or tool request.
type deepSeekBillingState struct {
	input        OpenAIRecordUsageInput
	revision     int
	modelCosts   []CostBreakdown
	accountCosts []float64
	baseRate     *float64
	reservations []*InflightReservation
}

func SetDeepSeekBillingInput(c *gin.Context, input *OpenAIRecordUsageInput) {
	if input != nil && input.APIKey != nil && input.APIKey.Group != nil && input.APIKey.Group.DeepSeekTools.Enabled {
		c.Set(deepSeekBillingInputKey, input)
	}
}

func (s *OpenAIGatewayService) BeginDeepSeekToolUsage(ctx context.Context, input *OpenAIRecordUsageInput) error {
	return s.beginDeepSeekToolUsageUntil(ctx, input, time.Now().Add(deepSeekRequestTimeout+time.Minute))
}

// beginDeepSeekToolUsageUntil opens the journal entry; recovery leaves a pending
// entry alone until deadline, which must outlive the supplier call.
func (s *OpenAIGatewayService) beginDeepSeekToolUsageUntil(ctx context.Context, input *OpenAIRecordUsageInput, deadline time.Time) error {
	repo, ok := s.usageBillingRepo.(DeepSeekUsageJournal)
	if !ok || input == nil || input.Result == nil || input.APIKey == nil || input.User == nil || input.Account == nil {
		return errors.New("DeepSeek tools require the durable usage journal")
	}
	if err := repo.BeginDeepSeekUsage(ctx, input.Result.RequestID, input.APIKey.ID, input.User.ID, input.Account.ID, deadline); err != nil {
		return err
	}
	input.Result.deepSeekBilling = &deepSeekBillingState{input: *input}
	return nil
}

func (s *OpenAIGatewayService) beginDeepSeekHostedUsage(ctx context.Context, c *gin.Context, key *APIKey, account *Account, result *OpenAIForwardResult) error {
	value, _ := c.Get(deepSeekBillingInputKey)
	input, _ := value.(*OpenAIRecordUsageInput)
	if input == nil {
		return errors.New("DeepSeek usage metadata is missing")
	}
	copy := *input
	copy.Result, copy.APIKey, copy.User, copy.Account = result, key, key.User, account
	return s.BeginDeepSeekToolUsage(ctx, &copy)
}

func (s *OpenAIGatewayService) checkpointDeepSeekUsage(ctx context.Context, result *OpenAIForwardResult) error {
	if result == nil || result.deepSeekBilling == nil {
		return errors.New("DeepSeek usage journal was not started")
	}
	input := result.deepSeekBilling.input
	input.Result = result
	input.deepSeekCheckpoint = true
	billingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	return s.RecordUsage(billingCtx, &input)
}

func (s *OpenAIGatewayService) recordDeepSeekUsage(ctx context.Context, input *OpenAIRecordUsageInput, usage *UsageLog, p *postUsageBillingParams) error {
	repo, ok := s.usageBillingRepo.(DeepSeekUsageJournal)
	state := input.Result.deepSeekBilling
	if !ok || state == nil {
		return errors.New("DeepSeek usage journal was not started")
	}
	if !input.deepSeekCheckpoint {
		defer func() {
			for _, reservation := range state.reservations {
				reservation.HandlerDone()
			}
		}()
	}
	cmd := buildUsageBillingCommand(usage.RequestID, usage, p)
	state.revision++
	if err := repo.SaveDeepSeekUsage(ctx, &DeepSeekUsageCheckpoint{Command: cmd, Usage: usage, Revision: state.revision, Ready: !input.deepSeekCheckpoint}); err != nil {
		return err
	}
	if input.deepSeekCheckpoint {
		return nil
	}
	settlement, err := repo.SettleDeepSeekUsage(ctx, usage.RequestID, usage.APIKeyID)
	if err != nil {
		return err
	}
	if settlement == nil {
		return errors.New("DeepSeek usage settlement is pending")
	}
	invalidateDeepSeekAuthCache(ctx, p.APIKey, settlement.Result, p.APIKeyService)
	if settlement.Result != nil && settlement.Result.Applied {
		finalizePostUsageBilling(ctx, p, s.billingDeps(), settlement.Result)
	}
	s.invalidateDeepSeekBillingCaches(ctx, cmd)
	return nil
}

// A replay may follow an ambiguous committed settlement, so invalidate then as
// well. Recovery knows user/key IDs only; secrets never enter the journal.
func invalidateDeepSeekAuthCache(ctx context.Context, key *APIKey, result *UsageBillingApplyResult, updater any) {
	if key == nil || result == nil || (result.Applied && !result.APIKeyQuotaExhausted) {
		return
	}
	if invalidator, ok := updater.(apiKeyAuthCacheInvalidator); ok && key.Key != "" {
		invalidator.InvalidateAuthCacheByKey(ctx, key.Key)
		return
	}
	if invalidator, ok := updater.(interface{ InvalidateAuthCacheByUserID(context.Context, int64) }); ok && key.UserID > 0 {
		invalidator.InvalidateAuthCacheByUserID(ctx, key.UserID)
	}
}

func (s *OpenAIGatewayService) invalidateDeepSeekBillingCaches(ctx context.Context, cmd *UsageBillingCommand) {
	if cmd == nil || s.billingCacheService == nil {
		return
	}
	_ = s.billingCacheService.InvalidateUserBalance(ctx, cmd.UserID)
	_ = s.billingCacheService.InvalidateAPIKeyRateLimit(ctx, cmd.APIKeyID)
}

func (s *OpenAIGatewayService) reserveDeepSeekTool(ctx context.Context, key *APIKey, result *OpenAIForwardResult, price float64) error {
	if s.billingCacheService == nil {
		return errors.New("DeepSeek tool billing service unavailable")
	}
	// Reuse the fixed-charge admission path, including authoritative Key quota
	// checks. Cache failures retain the existing documented fail-open semantics.
	reservation, err := s.billingCacheService.ReserveFixedCharge(ctx, key.User, key, key.Group, nil, price)
	if err != nil {
		return err
	}
	if reservation != nil {
		result.deepSeekBilling.reservations = append(result.deepSeekBilling.reservations, reservation)
	}
	return nil
}
