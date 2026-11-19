//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// The identifiers below are fixtures. No supplier, bill or customer is real.
const (
	traceFixtureClientID  = "5b0d7c1e-2f3a-4b6c-8d9e-0a1b2c3d4e5f"
	traceFixtureRequestID = "req-fixture-0001"
	traceFixtureRay       = "8f1c2d3e4a5b6c7d-FIX"
)

func traceFixtureHeaders() http.Header {
	h := http.Header{}
	h.Set("X-Client-Request-ID", traceFixtureClientID)
	h.Set("X-Request-ID", traceFixtureRequestID)
	h.Set("CF-Ray", traceFixtureRay)
	h.Set("Set-Cookie", "session=must-never-be-kept")
	return h
}

func TestUpstreamRejectionTraceValueKeepsOnlyStorableIdentifiers(t *testing.T) {
	require.Equal(t, "abc-123", upstreamRejectionTraceValue("  abc-123\t"))
	require.Empty(t, upstreamRejectionTraceValue(""))
	require.Empty(t, upstreamRejectionTraceValue("   "))
	// A value PostgreSQL JSONB or the validator would refuse is dropped whole.
	for _, bad := range []string{"a\x00b", "a\nb", "a\x7fb", "标识", "caf\xc3\xa9", "\xff\xfe"} {
		require.Empty(t, upstreamRejectionTraceValue(bad), "%q", bad)
	}
	// An identifier that does not fit is dropped too: a cut one matches nothing.
	exact := strings.Repeat("a", maxUpstreamRejectionTraceValueLen)
	require.Equal(t, exact, upstreamRejectionTraceValue(exact))
	long := exact + "a"
	require.Empty(t, upstreamRejectionTraceValue(long))
	// Whatever the sanitizer returns, the validator accepts.
	for _, v := range []string{"", "x", exact, long, " a b ", "a\x00b", traceFixtureRay} {
		require.True(t, validUpstreamRejectionTraceValue(upstreamRejectionTraceValue(v)), "%q", v)
	}
	require.False(t, validUpstreamRejectionTraceValue(long))
	require.False(t, validUpstreamRejectionTraceValue("a\x00b"))
}

func TestUpstreamRejectionStorableRequestIDKeepsTheLedgerFormUnlessStorageRefusesIt(t *testing.T) {
	// Ordinary ids, including non-ASCII ones, are stored as they always were.
	for _, id := range []string{"req_123", "标识-7", "a b", "caf\u00e9"} {
		require.Equal(t, id, storableUpstreamRequestID(id), "%q", id)
	}
	require.Equal(t, "req_123", storableUpstreamRequestID("  req_123 "))
	require.Empty(t, storableUpstreamRequestID(""))
	// JSONB cannot store these; the id is dropped rather than the settlement.
	for _, bad := range []string{"a\x00b", "a\nb", "a\rb", "a\x1bb", "a\x7fb", "\xff\xfe", "ok\xc3"} {
		require.Empty(t, storableUpstreamRequestID(bad), "%q", bad)
	}
	// Longer than the usage column: cut exactly as the usage row cuts it.
	long := strings.Repeat("x", maxUsageUpstreamRequestIDLen+50)
	require.Equal(t, long[:maxUsageUpstreamRequestIDLen], storableUpstreamRequestID(long))
	account := &Account{Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Supplier-Id"}}
	for _, value := range []string{long, strings.Repeat("标", 100), "req_123", "标识-7"} {
		h := http.Header{}
		h.Set("X-Supplier-Id", value)
		usage := usageUpstreamRequestIDPtr(account, h, false)
		require.NotNil(t, usage)
		require.Equal(t, *usage, storableUpstreamRequestID(value), "the ledger and the usage row agree")
		require.LessOrEqual(t, len(storableUpstreamRequestID(value)), maxUsageUpstreamRequestIDLen)
	}
}

func TestUpstreamRejectionTraceReadsFixedHeadersAndTheAccountHeader(t *testing.T) {
	account := &Account{Platform: PlatformGrok, Type: AccountTypeAPIKey}
	got := upstreamRejectionTraceFromHeaders(account, traceFixtureHeaders())
	require.Equal(t, upstreamRejectionTrace{XClientRequestID: traceFixtureClientID, XRequestID: traceFixtureRequestID, CFRay: traceFixtureRay}, got,
		"without an account header only the fixed identifiers are read")

	account.Extra = map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Client-Request-ID"}
	got = upstreamRejectionTraceFromHeaders(account, traceFixtureHeaders())
	require.Equal(t, traceFixtureClientID, got.UpstreamRequestID)

	require.Equal(t, upstreamRejectionTrace{}, upstreamRejectionTraceFromHeaders(account, nil))
	require.Equal(t, upstreamRejectionTrace{}, upstreamRejectionTraceFromHeaders(nil, http.Header{"Content-Type": {"text/html"}}))
}

func TestUpstreamRejectionTraceFillNeverReplacesExistingEvidence(t *testing.T) {
	e := UpstreamRejectionBillingEvidence{OriginalStatus: 403, UpstreamRequestID: "first", XRequestID: "kept"}
	upstreamRejectionTrace{Status: 524, UpstreamRequestID: "second", XClientRequestID: traceFixtureClientID, XRequestID: "other", CFRay: traceFixtureRay}.fill(&e)
	require.Equal(t, UpstreamRejectionBillingEvidence{OriginalStatus: 403, UpstreamRequestID: "first", XRequestID: "kept",
		XClientRequestID: traceFixtureClientID, CFRay: traceFixtureRay}, e)

	// The deadline release keeps the observation and stays a deadline release.
	waiver := NewUpstreamRejectionDeadlineReleaseCommand(NewUpstreamRejectionRequestID(), 7).Evidence
	kept := waiver.WithUpstreamObservation(UpstreamRejectionBillingEvidence{OriginalStatus: 524, CFRay: traceFixtureRay, RuleID: "not-an-observation", DurationMs: 9})
	require.Equal(t, UpstreamRejectionBillingEvidence{ReleaseReason: UpstreamRejectionReleaseDeadlinePassed, OriginalStatus: 524, CFRay: traceFixtureRay}, kept)
	require.Equal(t, waiver, waiver.WithUpstreamObservation(UpstreamRejectionBillingEvidence{}))
}

func TestUpstreamRejectionEvidenceIdentifiersAreValidatedAndOmittedWhenEmpty(t *testing.T) {
	id := NewUpstreamRejectionRequestID()
	unknown := &UpstreamRejectionFinalizeCommand{RequestID: id, APIKeyID: 7, Outcome: UpstreamRejectionUnknown,
		Evidence: UpstreamRejectionBillingEvidence{OriginalStatus: 524, CFRay: traceFixtureRay}}
	require.NoError(t, unknown.Validate())
	encoded, err := json.Marshal(unknown)
	require.NoError(t, err)
	require.Equal(t, `{"request_id":"`+id+`","api_key_id":7,"outcome":"unknown","evidence":{"original_status":524,"cf_ray":"`+traceFixtureRay+`"}}`, string(encoded))

	// A command without identifiers is byte-identical to one built before they existed.
	plain, err := json.Marshal(&UpstreamRejectionFinalizeCommand{RequestID: id, APIKeyID: 7, Outcome: UpstreamRejectionUnknown})
	require.NoError(t, err)
	require.Equal(t, `{"request_id":"`+id+`","api_key_id":7,"outcome":"unknown","evidence":{}}`, string(plain))

	for name, mutate := range map[string]func(*UpstreamRejectionBillingEvidence){
		"control byte": func(e *UpstreamRejectionBillingEvidence) { e.XClientRequestID = "a\x00b" },
		"non ascii":    func(e *UpstreamRejectionBillingEvidence) { e.XRequestID = "标识" },
		"too long": func(e *UpstreamRejectionBillingEvidence) {
			e.CFRay = strings.Repeat("r", maxUpstreamRejectionTraceValueLen+1)
		},
		"newline in id": func(e *UpstreamRejectionBillingEvidence) { e.XRequestID = "a\r\nb" },
	} {
		cmd := *unknown
		mutate(&cmd.Evidence)
		require.ErrorIs(t, cmd.Validate(), ErrUpstreamRejectionInvalid, name)
	}

	// The deadline release is built by the server and carries no observation:
	// the repository copies it from the locked row.
	for _, mutate := range []func(*UpstreamRejectionBillingEvidence){
		func(e *UpstreamRejectionBillingEvidence) { e.XClientRequestID = traceFixtureClientID },
		func(e *UpstreamRejectionBillingEvidence) { e.XRequestID = traceFixtureRequestID },
		func(e *UpstreamRejectionBillingEvidence) { e.CFRay = traceFixtureRay },
		func(e *UpstreamRejectionBillingEvidence) { e.OriginalStatus = 524 },
	} {
		waiver := NewUpstreamRejectionDeadlineReleaseCommand(id, 7)
		mutate(&waiver.Evidence)
		require.ErrorIs(t, waiver.Validate(), ErrUpstreamRejectionInvalid)
	}
}

// tracingRejectionRepo validates like the real repository and keeps every
// finalize command, so a test can read the evidence that would be stored.
type tracingRejectionRepo struct {
	*rejectionFacadeRepo
	cmds []UpstreamRejectionFinalizeCommand
}

func (r *tracingRejectionRepo) FinalizeUpstreamRejection(ctx context.Context, c *UpstreamRejectionFinalizeCommand) (*UpstreamRejectionBillingResult, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	r.cmds = append(r.cmds, *c)
	return r.rejectionFacadeRepo.FinalizeUpstreamRejection(ctx, c)
}

func (r *tracingRejectionRepo) last(t *testing.T) UpstreamRejectionFinalizeCommand {
	t.Helper()
	require.NotEmpty(t, r.cmds)
	return r.cmds[len(r.cmds)-1]
}

// beginTracedRejection reserves and marks sent, and returns the context that
// carries the attempt, as the handlers pass it down to the transport seam.
func beginTracedRejection(t *testing.T, account *Account) (*OpenAIGatewayService, *tracingRejectionRepo, *UpstreamRejectionBillingAttempt, context.Context) {
	t.Helper()
	s, base, k, a := rejectionFacadeFixture()
	if account != nil {
		a = account
	}
	repo := &tracingRejectionRepo{rejectionFacadeRepo: base}
	s.usageBillingRepo = repo
	attempt, err := s.BeginUpstreamRejectionBilling(context.Background(), k, a, "grok-4.7", []byte(`{"model":"grok-4.7"}`))
	require.NoError(t, err)
	ctx := WithUpstreamRejectionBillingAttempt(context.Background(), attempt)
	require.NoError(t, s.MarkUpstreamRejectionBillingSent(ctx))
	return s, repo, attempt, ctx
}

func TestUpstreamRejectionObserveRecordsTheOneResponseOfAnAttempt(t *testing.T) {
	_, _, attempt, ctx := beginTracedRejection(t, nil)
	observeUpstreamRejectionResponse(ctx, nil) // a transport error has no response
	require.Nil(t, attempt.trace)

	observeUpstreamRejectionResponse(ctx, &http.Response{StatusCode: 524, Header: traceFixtureHeaders()})
	require.Equal(t, &upstreamRejectionTrace{Status: 524, XClientRequestID: traceFixtureClientID, XRequestID: traceFixtureRequestID, CFRay: traceFixtureRay}, attempt.trace)

	// An attempt sends once; a second response can never rewrite the first.
	observeUpstreamRejectionResponse(ctx, &http.Response{StatusCode: 200, Header: http.Header{"Cf-Ray": {"other"}}})
	require.Equal(t, 524, attempt.trace.Status)
	require.Equal(t, traceFixtureRay, attempt.trace.CFRay)

	// A request without a hold, or with the typed nil the handlers store when
	// rejection billing is off, is a no-op.
	observeUpstreamRejectionResponse(context.Background(), &http.Response{StatusCode: 200})
	var none *UpstreamRejectionBillingAttempt
	observeUpstreamRejectionResponse(WithUpstreamRejectionBillingAttempt(context.Background(), none), &http.Response{StatusCode: 200})
}

func TestUpstreamRejectionUnknownOutcomeKeepsWhatTheUpstreamAnswered(t *testing.T) {
	onlyRay := http.Header{}
	onlyRay.Set("CF-Ray", traceFixtureRay)
	all := UpstreamRejectionBillingEvidence{XClientRequestID: traceFixtureClientID, XRequestID: traceFixtureRequestID, CFRay: traceFixtureRay}
	withStatus := func(status int) UpstreamRejectionBillingEvidence {
		e := all
		e.OriginalStatus = status
		return e
	}
	cases := []struct {
		name     string
		response *http.Response
		err      error
		want     UpstreamRejectionBillingEvidence
	}{
		{"edge timeout page", &http.Response{StatusCode: 524, Header: onlyRay}, &UpstreamFailoverError{StatusCode: 524},
			UpstreamRejectionBillingEvidence{OriginalStatus: 524, CFRay: traceFixtureRay}},
		{"bad gateway from the supplier", &http.Response{StatusCode: 502, Header: traceFixtureHeaders()}, &UpstreamRejectionHTTPError{StatusCode: 502, Cause: errors.New("bad gateway")}, withStatus(502)},
		{"headers arrived then the body broke", &http.Response{StatusCode: 200, Header: traceFixtureHeaders()}, io.ErrUnexpectedEOF, withStatus(200)},
		// The gateway reports an empty 200 as a 502 of its own. The ledger keeps
		// what the upstream sent: this is the case it most plausibly billed.
		{"a 200 the gateway turned into a 502", &http.Response{StatusCode: 200, Header: traceFixtureHeaders()}, &UpstreamFailoverError{StatusCode: 502}, withStatus(200)},
		{"no response at all", nil, io.EOF, UpstreamRejectionBillingEvidence{}},
		// A transport failure is reported as 502 although nothing answered.
		{"a status no upstream sent is not stored", nil, &UpstreamFailoverError{StatusCode: http.StatusBadGateway}, UpstreamRejectionBillingEvidence{}},
		{"an error status without a response is not stored", nil, &UpstreamRejectionHTTPError{StatusCode: 500, Cause: errors.New("server error")}, UpstreamRejectionBillingEvidence{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, repo, attempt, ctx := beginTracedRejection(t, nil)
			observeUpstreamRejectionResponse(ctx, c.response)
			require.ErrorIs(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, nil, c.err), ErrUpstreamRejectionOutcomeUnknown)
			cmd := repo.last(t)
			require.Equal(t, UpstreamRejectionUnknown, cmd.Outcome)
			require.False(t, cmd.ConfirmedNoCharge)
			require.Equal(t, c.want, cmd.Evidence)
			require.Equal(t, UpstreamRejectionUnknown, repo.row.State)
			require.Zero(t, repo.captures, "an identifier never turns into a charge")
		})
	}
}

// An unknown outcome with nothing observed is the command that was written
// before identifiers existed, byte for byte. A transport failure reaches this
// code as the 502 the real forwarders return for it.
func TestUpstreamRejectionUnknownWithoutObservationKeepsTheEarlierCommandBytes(t *testing.T) {
	for _, failure := range []error{io.EOF, &UpstreamFailoverError{StatusCode: http.StatusBadGateway, ResponseBody: []byte("transport")}} {
		s, repo, attempt, _ := beginTracedRejection(t, nil)
		require.ErrorIs(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, nil, failure), ErrUpstreamRejectionOutcomeUnknown)
		cmd := repo.last(t)
		encoded, err := json.Marshal(&cmd)
		require.NoError(t, err)
		require.Equal(t, `{"request_id":"`+attempt.Reservation.RequestID+`","api_key_id":2,"outcome":"unknown","evidence":{}}`, string(encoded))
	}
}

func TestUpstreamRejectionPartialResultStaysUnknownWithIdentifiers(t *testing.T) {
	account := &Account{ID: 3, Platform: PlatformGrok, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://supplier.invalid", "api_key": "fixture-only"},
		Extra: map[string]any{"upstream_rejection_profile": UpstreamRejectionProfileGrokContentV1, "upstream_rejection_cost": 0.06,
			AccountExtraUpstreamRequestIDHeader: "X-Client-Request-ID"}}
	s, repo, attempt, ctx := beginTracedRejection(t, account)
	observeUpstreamRejectionResponse(ctx, &http.Response{StatusCode: 200, Header: traceFixtureHeaders()})
	partial := &OpenAIForwardResult{Stream: true, Usage: OpenAIUsage{InputTokens: 10, OutputTokens: 2}, UpstreamHeaders: traceFixtureHeaders()}
	require.ErrorIs(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, partial, io.ErrUnexpectedEOF), ErrUpstreamRejectionOutcomeUnknown)
	require.Equal(t, UpstreamRejectionBillingEvidence{OriginalStatus: 200, UpstreamRequestID: traceFixtureClientID,
		XClientRequestID: traceFixtureClientID, XRequestID: traceFixtureRequestID, CFRay: traceFixtureRay}, repo.last(t).Evidence)

	// Without the seam (a result built elsewhere), the result's headers still identify the request.
	s, repo, attempt, _ = beginTracedRejection(t, account)
	require.ErrorIs(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, partial, nil), ErrUpstreamRejectionOutcomeUnknown)
	require.Equal(t, UpstreamRejectionBillingEvidence{UpstreamRequestID: traceFixtureClientID,
		XClientRequestID: traceFixtureClientID, XRequestID: traceFixtureRequestID, CFRay: traceFixtureRay}, repo.last(t).Evidence)
}

func TestUpstreamRejectionFreeReleaseKeepsStatusAndIdentifiers(t *testing.T) {
	// The upstream refused the request without charging: released, as before,
	// now with the identifiers of that response.
	for _, status := range []int{400, 401, 403, 404, 422, 429} {
		s, repo, attempt, ctx := beginTracedRejection(t, nil)
		observeUpstreamRejectionResponse(ctx, &http.Response{StatusCode: status, Header: traceFixtureHeaders()})
		failure := &UpstreamFailoverError{StatusCode: status}
		require.ErrorIs(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, nil, failure), failure)
		cmd := repo.last(t)
		require.Equal(t, UpstreamRejectionReleased, cmd.Outcome)
		require.True(t, cmd.ConfirmedNoCharge)
		require.Equal(t, UpstreamRejectionBillingEvidence{OriginalStatus: status, XClientRequestID: traceFixtureClientID, XRequestID: traceFixtureRequestID, CFRay: traceFixtureRay}, cmd.Evidence)
	}

	// Decided on a status whose response was not seen: the earlier command, byte for byte.
	s, repo, attempt, _ := beginTracedRejection(t, nil)
	failure := &UpstreamFailoverError{StatusCode: 403}
	require.ErrorIs(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, nil, failure), failure)
	cmd := repo.last(t)
	encoded, err := json.Marshal(&cmd)
	require.NoError(t, err)
	require.Equal(t, `{"request_id":"`+attempt.Reservation.RequestID+`","api_key_id":2,"outcome":"released","confirmed_no_charge":true,"evidence":{"original_status":403}}`, string(encoded))

	// Never sent: nothing was observed and the command is the earlier one.
	s, base, k, a := rejectionFacadeFixture()
	repo = &tracingRejectionRepo{rejectionFacadeRepo: base}
	s.usageBillingRepo = repo
	attempt, err = s.BeginUpstreamRejectionBilling(context.Background(), k, a, "grok-4.7", []byte(`{"model":"grok-4.7"}`))
	require.NoError(t, err)
	local := errors.New("local request validation failed")
	require.ErrorIs(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, nil, local), local)
	cmd = repo.last(t)
	encoded, err = json.Marshal(&cmd)
	require.NoError(t, err)
	require.Equal(t, `{"request_id":"`+attempt.Reservation.RequestID+`","api_key_id":2,"outcome":"released","confirmed_no_charge":true,"evidence":{}}`, string(encoded))
}

func traceFixtureAccountWithRequestIDHeader() *Account {
	return &Account{ID: 3, Platform: PlatformGrok, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://supplier.invalid", "api_key": "fixture-only"},
		Extra: map[string]any{"upstream_rejection_profile": UpstreamRejectionProfileGrokContentV1, "upstream_rejection_cost": 0.06,
			AccountExtraUpstreamRequestIDHeader: "X-Client-Request-ID"}}
}

// The attempt settles a rejection; if that write is lost, the usage task
// settles it again from the result alone. Both must send the same command, or
// the retry conflicts with the charge it is repeating.
func TestUpstreamRejectionChargeEvidenceCarriesIdentifiersOnBothSettlementPaths(t *testing.T) {
	account := traceFixtureAccountWithRequestIDHeader()
	s, repo, attempt, ctx := beginTracedRejection(t, account)
	s.usageLogRepo = &openAIRecordUsageLogRepoStub{inserted: true}
	observeUpstreamRejectionResponse(ctx, &http.Response{StatusCode: 200, Header: traceFixtureHeaders()})
	ev := &UpstreamRejectionEvidence{Profile: UpstreamRejectionProfileID(), RuleID: "refusal_phrase_1", RuleVersion: UpstreamRejectionRuleVersion(), HTTPStatus: 200, ResponseHash: strings.Repeat("a", 64)}
	res := &OpenAIForwardResult{Model: "grok-4.7", UpstreamRejection: ev, UpstreamHeaders: traceFixtureHeaders(), Duration: 1500 * time.Millisecond}

	// The attempt's write fails after the command was built.
	repo.finalizeErr = errors.New("ledger temporarily unavailable")
	require.Error(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, res, nil))
	fromAttempt := repo.last(t)
	require.Zero(t, repo.captures)

	// The usage task repeats it through recordUpstreamRejectionUsage.
	repo.finalizeErr = nil
	key := attempt.APIKey
	require.NoError(t, s.RecordUsage(context.Background(), &OpenAIRecordUsageInput{Result: res, APIKey: key, User: key.User, Account: account}))
	fromUsageTask := repo.last(t)
	require.Len(t, repo.cmds, 2)
	require.Equal(t, fromAttempt, fromUsageTask)
	require.Equal(t, fromAttempt.Fingerprint(), fromUsageTask.Fingerprint())
	require.Equal(t, 1, repo.captures)

	cmd := fromUsageTask
	require.Equal(t, UpstreamRejectionRejected, cmd.Outcome)
	require.Equal(t, traceFixtureClientID, cmd.Evidence.UpstreamRequestID, "the account-declared header")
	require.Equal(t, traceFixtureClientID, cmd.Evidence.XClientRequestID)
	require.Equal(t, traceFixtureRequestID, cmd.Evidence.XRequestID)
	require.Equal(t, traceFixtureRay, cmd.Evidence.CFRay)
	require.Equal(t, 200, cmd.Evidence.OriginalStatus)
	require.Equal(t, "refusal_phrase_1", cmd.Evidence.RuleID)

	// A WebSocket turn has no HTTP response headers to read.
	ws := *res
	ws.OpenAIWSMode = true
	wsEvidence := rejectionBillingEvidence(account, &ws, attempt.Reservation.Model)
	require.Empty(t, wsEvidence.UpstreamRequestID+wsEvidence.XClientRequestID+wsEvidence.XRequestID+wsEvidence.CFRay)
}

// No response header, however broken, may cost a settlement. Every outcome is
// written through a repository that validates like the real one; what cannot be
// stored is dropped and the money decision is unchanged.
func TestUpstreamRejectionHostileHeadersNeverFailASettlement(t *testing.T) {
	hostile := func() http.Header {
		h := http.Header{}
		h["X-Client-Request-Id"] = []string{"id\x00with-nul"}
		h["X-Request-Id"] = []string{strings.Repeat("r", 4096)}
		h["Cf-Ray"] = []string{"ray\r\nInjected: 1"}
		h["X-Supplier-Id"] = []string{"\xff\xfe\x00broken"}
		return h
	}
	account := traceFixtureAccountWithRequestIDHeader()
	account.Extra[AccountExtraUpstreamRequestIDHeader] = "X-Supplier-Id"
	storable := func(t *testing.T, cmd UpstreamRejectionFinalizeCommand) {
		t.Helper()
		require.NoError(t, cmd.Validate())
		encoded, err := json.Marshal(&cmd)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), `\u0000`, "JSONB refuses a NUL")
		require.NotContains(t, string(encoded), `\ufffd`, "no invalid UTF-8 was papered over")
		require.Empty(t, cmd.Evidence.UpstreamRequestID+cmd.Evidence.XClientRequestID+cmd.Evidence.XRequestID+cmd.Evidence.CFRay)
	}

	t.Run("unknown", func(t *testing.T) {
		s, repo, attempt, ctx := beginTracedRejection(t, account)
		observeUpstreamRejectionResponse(ctx, &http.Response{StatusCode: 524, Header: hostile()})
		require.ErrorIs(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, nil, &UpstreamFailoverError{StatusCode: 524}), ErrUpstreamRejectionOutcomeUnknown)
		storable(t, repo.last(t))
		require.Equal(t, 524, repo.last(t).Evidence.OriginalStatus)
	})
	t.Run("partial result", func(t *testing.T) {
		s, repo, attempt, ctx := beginTracedRejection(t, account)
		observeUpstreamRejectionResponse(ctx, &http.Response{StatusCode: 200, Header: hostile()})
		partial := &OpenAIForwardResult{Stream: true, UpstreamHeaders: hostile()}
		require.ErrorIs(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, partial, io.ErrUnexpectedEOF), ErrUpstreamRejectionOutcomeUnknown)
		storable(t, repo.last(t))
	})
	t.Run("free release", func(t *testing.T) {
		s, repo, attempt, ctx := beginTracedRejection(t, account)
		observeUpstreamRejectionResponse(ctx, &http.Response{StatusCode: 429, Header: hostile()})
		failure := &UpstreamFailoverError{StatusCode: 429}
		require.ErrorIs(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, nil, failure), failure)
		storable(t, repo.last(t))
		require.Equal(t, UpstreamRejectionReleased, repo.last(t).Outcome)
	})
	t.Run("charged rejection", func(t *testing.T) {
		// The account-declared header used to reach the ledger unchecked here:
		// a NUL in it failed the write and the fee was never captured.
		s, repo, attempt, ctx := beginTracedRejection(t, account)
		observeUpstreamRejectionResponse(ctx, &http.Response{StatusCode: 200, Header: hostile()})
		ev := &UpstreamRejectionEvidence{Profile: UpstreamRejectionProfileID(), RuleID: "refusal_phrase_1", RuleVersion: UpstreamRejectionRuleVersion(), HTTPStatus: 200, ResponseHash: strings.Repeat("a", 64)}
		res := &OpenAIForwardResult{Model: "grok-4.7", UpstreamRejection: ev, UpstreamHeaders: hostile()}
		require.NoError(t, s.FinishUpstreamRejectionBillingAttempt(context.Background(), attempt, res, nil))
		storable(t, repo.last(t))
		require.Equal(t, UpstreamRejectionRejected, repo.last(t).Outcome)
		require.Equal(t, 1, repo.captures, "the fee is captured")
	})
}

// The transport path records the response while the handler path may already
// be settling (a client that went away). Run with -race.
func TestUpstreamRejectionObservationIsSafeAcrossGoroutines(t *testing.T) {
	_, _, attempt, ctx := beginTracedRejection(t, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(status int) {
			defer wg.Done()
			observeUpstreamRejectionResponse(ctx, &http.Response{StatusCode: status, Header: traceFixtureHeaders()})
		}(500 + i)
		go func() {
			defer wg.Done()
			e := attempt.observedEvidence(nil)
			// Either nothing yet, or one complete observation; never a mix.
			if e.OriginalStatus != 0 && (e.CFRay != traceFixtureRay || e.XClientRequestID != traceFixtureClientID) {
				t.Errorf("partial observation: %+v", e)
			}
		}()
	}
	wg.Wait()
	first := attempt.observedEvidence(nil)
	require.GreaterOrEqual(t, first.OriginalStatus, 500)
	observeUpstreamRejectionResponse(ctx, &http.Response{StatusCode: 200})
	require.Equal(t, first, attempt.observedEvidence(nil), "the first response stays")
}

// The real forwarding functions with a fake transport: the edge answers 524 for
// the origin, or the stream breaks after its headers. Either way the hold stays
// unknown, nothing is charged or retried, and the ledger command identifies the
// request.
func TestUpstreamRejectionTransportSeamFeedsTheLedgerEvidence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	begin := func(t *testing.T, upstream *httpUpstreamRecorder) (*OpenAIGatewayService, *tracingRejectionRepo, *UpstreamRejectionBillingAttempt, context.Context, *Account) {
		t.Helper()
		_, base, k, _ := rejectionFacadeFixture()
		repo := &tracingRejectionRepo{rejectionFacadeRepo: base}
		gateway := rejectionGatewayService(upstream)
		gateway.usageBillingRepo = repo
		account := rejectionGatewayTestAccount()
		attempt, err := gateway.BeginUpstreamRejectionBilling(context.Background(), k, account, rejectionGatewayTestModel, []byte(`{"model":"grok-4.7"}`))
		require.NoError(t, err)
		return gateway, repo, attempt, WithUpstreamRejectionBillingAttempt(context.Background(), attempt), account
	}

	t.Run("edge 524 page", func(t *testing.T) {
		page := rejectionGatewayResponse(524, "text/html", "<html>error code: 524</html>")
		page.Header.Del("X-Request-Id")
		page.Header.Set("CF-Ray", traceFixtureRay)
		upstream := &httpUpstreamRecorder{resp: page}
		gateway, repo, attempt, ctx, account := begin(t, upstream)
		request := []byte(`{"model":"grok-4.7","messages":[{"role":"user","content":"hello"}],"stream":false}`)
		c, _ := rejectionGatewayContext("/v1/chat/completions", request)
		c.Request = c.Request.WithContext(ctx)
		result, err := gateway.forwardAsRawChatCompletions(ctx, c, account, request, "")
		require.Error(t, err)
		require.Len(t, upstream.requests, 1)
		require.ErrorIs(t, gateway.FinishUpstreamRejectionBillingAttempt(ctx, attempt, result, err), ErrUpstreamRejectionOutcomeUnknown)
		cmd := repo.last(t)
		require.Equal(t, UpstreamRejectionUnknown, cmd.Outcome)
		require.Equal(t, UpstreamRejectionBillingEvidence{OriginalStatus: 524, CFRay: traceFixtureRay}, cmd.Evidence)
		require.Zero(t, repo.captures)
		require.Len(t, upstream.requests, 1, "an unknown outcome is never replayed")
	})

	t.Run("stream breaks after headers", func(t *testing.T) {
		prefix := `data: {"model":"grok-4.7","choices":[{"index":0,"delta":{"role":"assistant","content":"Working on it"}}]}` + "\n\n"
		resp := rejectionGatewayResponse(http.StatusOK, "text/event-stream", prefix)
		resp.Header.Set("X-Client-Request-ID", traceFixtureClientID)
		resp.Body = io.NopCloser(io.MultiReader(strings.NewReader(prefix), passthroughErrReadCloser{err: io.ErrUnexpectedEOF}))
		upstream := &httpUpstreamRecorder{resp: resp}
		gateway, repo, attempt, ctx, account := begin(t, upstream)
		request := []byte(`{"model":"grok-4.7","messages":[{"role":"user","content":"hello"}],"stream":true}`)
		c, _ := rejectionGatewayContext("/v1/chat/completions", request)
		c.Request = c.Request.WithContext(ctx)
		result, err := gateway.forwardAsRawChatCompletions(ctx, c, account, request, "")
		require.ErrorIs(t, gateway.FinishUpstreamRejectionBillingAttempt(ctx, attempt, result, err), ErrUpstreamRejectionOutcomeUnknown)
		cmd := repo.last(t)
		require.Equal(t, UpstreamRejectionUnknown, cmd.Outcome)
		require.Equal(t, UpstreamRejectionBillingEvidence{OriginalStatus: 200, XClientRequestID: traceFixtureClientID, XRequestID: "supplier-refusal-fixture"}, cmd.Evidence)
		require.Zero(t, repo.captures)
	})
}

// The media creation request sends through its own transport call. It records
// the response the same way.
func TestUpstreamRejectionMediaSeamFeedsTheLedgerEvidence(t *testing.T) {
	svc, base, key, account := rejectionFacadeFixture()
	repo := &tracingRejectionRepo{rejectionFacadeRepo: base}
	svc.usageBillingRepo = repo
	account.ID = 900502 // isolate ordinary cooldown state from other fixtures
	svc.cfg = &config.Config{}
	account.Credentials["pool_mode"] = false
	response := grokMediaRejectionTestResponse(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`)
	response.Header.Set("CF-Ray", traceFixtureRay)
	upstream := &grokMediaContentUpstreamStub{response: response}
	svc.httpUpstream = upstream
	body := []byte(`{"model":"grok-imagine-image","prompt":"A green square"}`)
	attempt, err := svc.BeginUpstreamRejectionBilling(context.Background(), key, account, "grok-imagine-image", body)
	require.NoError(t, err)
	ctx := WithUpstreamRejectionBillingAttempt(context.Background(), attempt)
	c, _ := grokMediaContentTestContext(http.MethodPost, "https://local.example/v1/images/generations", nil)
	result, forwardErr := svc.ForwardGrokMedia(ctx, c, account, GrokMediaEndpointImagesGenerations, "", body, "application/json")
	require.Nil(t, result)
	require.Error(t, forwardErr)
	require.ErrorIs(t, svc.FinishUpstreamRejectionBillingAttempt(ctx, attempt, result, forwardErr), ErrUpstreamRejectionOutcomeUnknown)
	cmd := repo.last(t)
	require.Equal(t, UpstreamRejectionUnknown, cmd.Outcome)
	require.Equal(t, UpstreamRejectionBillingEvidence{OriginalStatus: http.StatusBadGateway, XRequestID: "supplier-request-1", CFRay: traceFixtureRay}, cmd.Evidence)
	require.Zero(t, repo.captures)
	require.Len(t, upstream.requests, 1)
}
