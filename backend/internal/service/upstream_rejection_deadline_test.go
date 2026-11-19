//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUpstreamRejectionDeadlineKeepsExistingFingerprintsByteIdentical(t *testing.T) {
	id := NewUpstreamRejectionRequestID()
	cases := []struct {
		cmd  UpstreamRejectionFinalizeCommand
		want string
	}{
		{UpstreamRejectionFinalizeCommand{RequestID: id, APIKeyID: 7, Outcome: UpstreamRejectionReleased, ConfirmedNoCharge: true},
			`{"request_id":"` + id + `","api_key_id":7,"outcome":"released","confirmed_no_charge":true,"evidence":{}}`},
		{UpstreamRejectionFinalizeCommand{RequestID: id, APIKeyID: 7, Outcome: UpstreamRejectionUnknown},
			`{"request_id":"` + id + `","api_key_id":7,"outcome":"unknown","evidence":{}}`},
		{UpstreamRejectionFinalizeCommand{RequestID: id, APIKeyID: 7, Outcome: UpstreamRejectionRejected, Evidence: UpstreamRejectionBillingEvidence{RuleID: "r", RuleVersion: "v", OriginalStatus: 403, UpstreamRequestID: "u"}},
			`{"request_id":"` + id + `","api_key_id":7,"outcome":"rejected","evidence":{"rule_id":"r","rule_version":"v","original_status":403,"upstream_request_id":"u"}}`},
	}
	for _, c := range cases {
		encoded, err := json.Marshal(&c.cmd)
		require.NoError(t, err)
		require.Equal(t, c.want, string(encoded))
		require.Equal(t, HashUsageRequestPayload([]byte(c.want)), c.cmd.Fingerprint())
	}
	waiver := NewUpstreamRejectionDeadlineReleaseCommand(id, 7)
	require.NoError(t, waiver.Validate())
	require.NotEqual(t, cases[0].cmd.Fingerprint(), waiver.Fingerprint())
	encoded, err := json.Marshal(waiver)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"deadline_passed":true`)
	require.Contains(t, string(encoded), `"release_reason":"deadline_passed"`)
	// The reservation snapshot written into events does not gain the deadline.
	at := time.Now()
	snapshot, err := json.Marshal(&UpstreamRejectionReservation{ReleaseAfter: &at})
	require.NoError(t, err)
	require.NotContains(t, string(snapshot), "release")
}

func TestUpstreamRejectionDeadlineWaiverValidation(t *testing.T) {
	id := NewUpstreamRejectionRequestID()
	bad := []func(*UpstreamRejectionFinalizeCommand){
		func(c *UpstreamRejectionFinalizeCommand) { c.ConfirmedNoCharge = true },
		func(c *UpstreamRejectionFinalizeCommand) { c.Outcome = UpstreamRejectionUnknown },
		func(c *UpstreamRejectionFinalizeCommand) { c.Evidence.UpstreamRequestID = "extra" },
		func(c *UpstreamRejectionFinalizeCommand) { c.Evidence.ReleaseReason = "other" },
		func(c *UpstreamRejectionFinalizeCommand) { c.DeadlinePassed = false },
		func(c *UpstreamRejectionFinalizeCommand) { c.Evidence.ReleasedFrom = UpstreamRejectionSent },
	}
	for i, mutate := range bad {
		cmd := NewUpstreamRejectionDeadlineReleaseCommand(id, 7)
		mutate(cmd)
		require.ErrorIs(t, cmd.Validate(), ErrUpstreamRejectionInvalid, "case %d", i)
	}
	released := &UpstreamRejectionReservation{State: UpstreamRejectionReleased}
	require.False(t, released.ReleasedAtDeadline())
	released.Evidence.ReleaseReason = UpstreamRejectionReleaseDeadlinePassed
	require.True(t, released.ReleasedAtDeadline())
	require.False(t, released.ReleasedAtDeadlineWhileSent())
	released.Evidence.ReleasedFrom = UpstreamRejectionSent
	require.True(t, released.ReleasedAtDeadlineWhileSent())
	// No other command may carry the repository-owned released-from state.
	other := &UpstreamRejectionFinalizeCommand{RequestID: id, APIKeyID: 7, Outcome: UpstreamRejectionUnknown, Evidence: UpstreamRejectionBillingEvidence{ReleasedFrom: UpstreamRejectionUnknown}}
	require.ErrorIs(t, other.Validate(), ErrUpstreamRejectionInvalid)
}

type dueReleaserStub struct {
	rows      []UpstreamRejectionReservation
	finalize  func(context.Context, *UpstreamRejectionFinalizeCommand) (*UpstreamRejectionBillingResult, error)
	finalized []string
	lists     int
}

func (s *dueReleaserStub) ListDueUpstreamRejectionsPage(_ context.Context, q *UpstreamRejectionDueQuery) (*UpstreamRejectionDuePage, error) {
	s.lists++
	start := 0
	if q.After != nil {
		for i := range s.rows {
			if s.rows[i].RequestID == q.After.RequestID {
				start = i + 1
			}
		}
	}
	end := min(len(s.rows), start+q.Limit)
	page := &UpstreamRejectionDuePage{Items: append([]UpstreamRejectionReservation{}, s.rows[start:end]...)}
	if end-start == q.Limit {
		last := s.rows[end-1]
		page.Next = &UpstreamRejectionDueCursor{ReleaseAfter: *last.ReleaseAfter, RequestID: last.RequestID}
	}
	return page, nil
}

func (s *dueReleaserStub) FinalizeUpstreamRejection(ctx context.Context, c *UpstreamRejectionFinalizeCommand) (*UpstreamRejectionBillingResult, error) {
	if err := c.Validate(); err != nil || !c.DeadlinePassed {
		return nil, fmt.Errorf("unexpected command: %v", err)
	}
	s.finalized = append(s.finalized, c.RequestID)
	return s.finalize(ctx, c)
}

func dueRows(n int) []UpstreamRejectionReservation {
	rows := make([]UpstreamRejectionReservation, n)
	base := time.Now().Add(-time.Hour)
	for i := range rows {
		at := base.Add(time.Duration(i) * time.Second)
		rows[i] = UpstreamRejectionReservation{UpstreamRejectionReserveCommand: UpstreamRejectionReserveCommand{
			RequestID: NewUpstreamRejectionRequestID(), UserID: int64(100 + i), APIKeyID: int64(200 + i), Price: 0.1,
		}, State: UpstreamRejectionUnknown, ReleaseAfter: &at}
	}
	return rows
}

func applied(context.Context, *UpstreamRejectionFinalizeCommand) (*UpstreamRejectionBillingResult, error) {
	return &UpstreamRejectionBillingResult{Applied: true}, nil
}

func TestUpstreamRejectionDeadlineSweepCapsWorkPerRunAndResumes(t *testing.T) {
	stub := &dueReleaserStub{rows: dueRows(rejectionDeadlineReleaseMaxPerRun*2 + 5), finalize: applied}
	invalidated := 0
	w := &UpstreamRejectionRecovery{dueRepo: stub, invalidateDue: func(context.Context, *UpstreamRejectionReservation) { invalidated++ }}
	w.releaseDueReservations(context.Background())
	require.Len(t, stub.finalized, rejectionDeadlineReleaseMaxPerRun, "a backlog cannot starve task reconciliation")
	require.Equal(t, rejectionDeadlineReleaseMaxPerRun, invalidated)
	w.releaseDueReservations(context.Background())
	w.releaseDueReservations(context.Background())
	require.Len(t, stub.finalized, len(stub.rows))
	seen := map[string]bool{}
	for _, id := range stub.finalized {
		require.False(t, seen[id], "every due row is processed once per pass")
		seen[id] = true
	}
	require.Nil(t, w.dueCursor, "a finished pass restarts from the oldest deadline")
}

func TestUpstreamRejectionDeadlineSweepStopsPromptlyOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stub := &dueReleaserStub{rows: dueRows(10)}
	stub.finalize = func(context.Context, *UpstreamRejectionFinalizeCommand) (*UpstreamRejectionBillingResult, error) {
		if len(stub.finalized) == 3 {
			cancel()
		}
		return &UpstreamRejectionBillingResult{Applied: true}, nil
	}
	w := &UpstreamRejectionRecovery{dueRepo: stub}
	w.releaseDueReservations(ctx)
	require.Len(t, stub.finalized, 3)
	require.Nil(t, w.dueCursor, "a page interrupted mid-way does not advance the cursor")
	w.releaseDueReservations(ctx)
	require.Len(t, stub.finalized, 3, "a cancelled run does no work")
	require.Equal(t, 1, stub.lists)
	// The next run resumes the same page instead of skipping its remainder.
	stub.finalize = applied
	w.releaseDueReservations(context.Background())
	require.Len(t, stub.finalized, 3+len(stub.rows))
	require.Equal(t, stub.rows[3].RequestID, stub.finalized[6])
}

func TestUpstreamRejectionDeadlineSweepInvalidatesOnlyAppliedReleases(t *testing.T) {
	rows := dueRows(4)
	outcomes := map[string]func() (*UpstreamRejectionBillingResult, error){
		rows[0].RequestID: func() (*UpstreamRejectionBillingResult, error) {
			return &UpstreamRejectionBillingResult{Applied: true}, nil
		},
		rows[1].RequestID: func() (*UpstreamRejectionBillingResult, error) { return nil, ErrUpstreamRejectionState },
		rows[2].RequestID: func() (*UpstreamRejectionBillingResult, error) {
			return &UpstreamRejectionBillingResult{Applied: false}, nil
		},
		rows[3].RequestID: func() (*UpstreamRejectionBillingResult, error) { return nil, errors.New("database unavailable") },
	}
	stub := &dueReleaserStub{rows: rows, finalize: func(_ context.Context, c *UpstreamRejectionFinalizeCommand) (*UpstreamRejectionBillingResult, error) {
		return outcomes[c.RequestID]()
	}}
	var invalidated []int64
	w := &UpstreamRejectionRecovery{dueRepo: stub, invalidateDue: func(_ context.Context, r *UpstreamRejectionReservation) {
		invalidated = append(invalidated, r.UserID)
	}}
	w.releaseDueReservations(context.Background())
	require.Len(t, stub.finalized, 4, "a lost race or a failure does not stop the run")
	require.Equal(t, []int64{rows[0].UserID}, invalidated)
}

func TestUpstreamRejectionDeadlineUnsentRecoveryNeverReleasesSentHold(t *testing.T) {
	// The unsent recovery must still never release a sent hold.
	old := time.Now().Add(-time.Hour)
	rows := dueRows(1)
	rows[0].CreatedAt, rows[0].State = old, UpstreamRejectionSent
	released := 0
	w := &UpstreamRejectionRecovery{
		repo: &rejectionRecoverySourceStub{page: func(context.Context, *UpstreamRejectionPendingQuery) (*UpstreamRejectionPendingPage, error) {
			return &UpstreamRejectionPendingPage{Items: rows}, nil
		}},
		releaseUnsent: func(context.Context, *UpstreamRejectionReservation) error { released++; return nil },
	}
	w.recoverUnsentReservations(context.Background())
	require.Zero(t, released)
}

type lateResultRepo struct {
	rejectionFacadeRepo
	applies int
	lastCmd *UsageBillingCommand
}

func (r *lateResultRepo) Apply(_ context.Context, cmd *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	r.applies++
	r.lastCmd = cmd
	return nil, ErrUpstreamRejectionWaived
}

func TestUpstreamRejectionLateResultsAfterDeadlineAreTerminalAndUncharged(t *testing.T) {
	// Late rejection: the attempt and the usage task both treat it as settled.
	s, r, k, a, attempt := beginMarkedRejection(t)
	r.row.State, r.row.Evidence = UpstreamRejectionReleased, UpstreamRejectionBillingEvidence{ReleaseReason: UpstreamRejectionReleaseDeadlinePassed}
	r.finalizeErr = ErrUpstreamRejectionWaived
	logs := &openAIRecordUsageLogRepoStub{inserted: true}
	s.usageLogRepo = logs
	ev := &UpstreamRejectionEvidence{Profile: UpstreamRejectionProfileID(), RuleID: "text.refusal", RuleVersion: UpstreamRejectionRuleVersion(), HTTPStatus: 200, ResponseHash: strings.Repeat("a", 64)}
	res := &OpenAIForwardResult{Model: "grok-4.7", UpstreamRejection: ev, Usage: OpenAIUsage{InputTokens: 20, OutputTokens: 4}}
	require.NoError(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, res, nil))
	require.NoError(t, s.RecordUsage(context.Background(), &OpenAIRecordUsageInput{Result: res, APIKey: k, User: k.User, Account: a}))
	require.Zero(t, r.captures)
	require.Zero(t, logs.calls, "no charge row is projected for a waived hold")

	// An unknown outcome after the waiver keeps today's client-facing error.
	s, r, _, _, attempt = beginMarkedRejection(t)
	r.finalizeErr = ErrUpstreamRejectionWaived
	require.ErrorIs(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, &OpenAIForwardResult{}, nil), ErrUpstreamRejectionOutcomeUnknown)

	// Late ordinary usage: the repository records it; the usage task ends quietly.
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	svc.resolver = NewModelPricingResolver(nil, svc.billingService)
	group := &Group{ID: 2, Platform: PlatformOpenAI, RateMultiplier: 1}
	key := &APIKey{ID: 10, UserID: 20, GroupID: &group.ID, Group: group, User: &User{ID: 20}}
	account := &Account{ID: 30, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Request-ID"}}
	repo := &lateResultRepo{}
	repo.row = &UpstreamRejectionReservation{UpstreamRejectionReserveCommand: UpstreamRejectionReserveCommand{
		RequestID: NewUpstreamRejectionRequestID(), UserID: 20, APIKeyID: 10, AccountID: 30, GroupID: 2, Model: "gpt-6.1-sol",
	}, State: UpstreamRejectionReleased, Evidence: UpstreamRejectionBillingEvidence{ReleaseReason: UpstreamRejectionReleaseDeadlinePassed}}
	svc.usageBillingRepo = repo
	headers := make(map[string][]string)
	headers["X-Request-Id"] = []string{"supplier-side-id"}
	result := &OpenAIForwardResult{Model: "gpt-6.1-sol", UpstreamRejectionReservationID: repo.row.RequestID,
		Usage: OpenAIUsage{InputTokens: 1000, OutputTokens: 200}, UpstreamHeaders: headers}
	require.NoError(t, svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{Result: result, APIKey: key, User: key.User, Account: account}))
	require.Equal(t, 1, repo.applies)
	require.Equal(t, "supplier-side-id", repo.lastCmd.UpstreamRequestID)
	require.Nil(t, usageRepo.lastLog, "no usage row is written for an uncharged late result")
}

// disconnectedClientWriter is a client that went away: every write fails.
type disconnectedClientWriter struct{ header http.Header }

func (w *disconnectedClientWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *disconnectedClientWriter) Write([]byte) (int, error) { return 0, syscall.EPIPE }
func (w *disconnectedClientWriter) WriteHeader(int)           {}
func (w *disconnectedClientWriter) Flush()                    {}

// A client that disconnects before the response must not turn a supplier
// rejection into an unknown outcome released for free. The upstream request is
// detached from the client's context and the stream is drained to its terminal
// event, so the rejection is classified and the fixed fee is captured.
func TestUpstreamRejectionDisconnectedClientIsStillChargedTheFixedFee(t *testing.T) {
	gin.SetMode(gin.TestMode)
	chatRefusal := strings.Join([]string{
		`data: {"model":"grok-4.7","choices":[{"index":0,"delta":{"role":"assistant","content":"` + rejectionGatewayTestText + `"}}]}`, "",
		`data: {"model":"grok-4.7","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "",
		"data: [DONE]", "", "",
	}, "\n")
	for _, protocol := range []string{"responses", "chat_completions"} {
		t.Run(protocol, func(t *testing.T) {
			clientCtx, disconnect := context.WithCancel(context.Background())
			disconnect()
			c, _ := gin.CreateTestContext(&disconnectedClientWriter{})
			var request []byte
			var payload string
			if protocol == "responses" {
				request = []byte(`{"model":"grok-4.7","input":"hello","stream":true}`)
				payload = rejectionGatewayResponsesSSE()
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(request)).WithContext(clientCtx)
			} else {
				request = []byte(`{"model":"grok-4.7","messages":[{"role":"user","content":"hello"}],"stream":true}`)
				payload = chatRefusal
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(request)).WithContext(clientCtx)
			}
			c.Request.Header.Set("Content-Type", "application/json")
			upstream := &httpUpstreamRecorder{resp: rejectionGatewayResponse(http.StatusOK, "text/event-stream", payload)}
			gateway := rejectionGatewayService(upstream)
			var result *OpenAIForwardResult
			var err error
			if protocol == "responses" {
				result, err = gateway.forwardGrokResponses(clientCtx, c, rejectionGatewayTestAccount(), request, rejectionGatewayTestModel, true, time.Now())
			} else {
				result, err = gateway.forwardAsRawChatCompletions(clientCtx, c, rejectionGatewayTestAccount(), request, "")
			}
			require.Len(t, upstream.requests, 1)
			require.NoError(t, upstream.requests[0].Context().Err(), "the upstream call must outlive the client")
			require.NotNil(t, result)
			require.NotNil(t, result.UpstreamRejection, "the drained stream classifies the rejection")
			requireNoRejectionFailover(t, err)

			s, repo, _, _, attempt := beginMarkedRejection(t)
			_ = s.FinishUpstreamRejectionBillingAttempt(clientCtx, attempt, result, err)
			require.Equal(t, 1, repo.captures, "a disconnecting client still pays the fixed fee")
			require.Equal(t, UpstreamRejectionRejected, repo.row.State)
			require.NoError(t, repo.lastCtxErr, "settlement uses a context detached from the client")
		})
	}
}
