//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const composerBridgeTestModel = "grok-composer-2.5-fast"

// A Grok API-key account without a mapping of its own serves the composer
// names through the default Grok mapping, so a rejection-billed group reaches
// the image bridge with no extra configuration.
func composerBridgeRejectionAccount() *Account {
	a := rejectionGatewayTestAccount()
	delete(a.Credentials, "model_mapping")
	delete(a.Credentials, "pool_mode")
	delete(a.Credentials, "pool_mode_retry_count")
	delete(a.Credentials, "pool_mode_retry_status_codes")
	return a
}

const composerBridgeTestImage = `{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}`

func composerBridgeChatRequest(model string, stream bool, imageParts ...string) []byte {
	parts := append([]string{`{"type":"text","text":"What is shown?"}`}, imageParts...)
	streamValue := "false"
	if stream {
		streamValue = "true"
	}
	return []byte(`{"model":"` + model + `","messages":[{"role":"user","content":[` + strings.Join(parts, ",") + `]}],"stream":` + streamValue + `}`)
}

func composerBridgeDescriptionResponse() *http.Response {
	return rejectionGatewayResponse(http.StatusOK, "application/json",
		`{"id":"resp_vision","object":"response","model":"grok-build-0.1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"A small diagram with ABC letters."}]}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}`)
}

func composerBridgeChatResponse(model string) *http.Response {
	return rejectionGatewayResponse(http.StatusOK, "application/json",
		`{"id":"chatcmpl_composer","object":"chat.completion","model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":"It shows ABC."},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`)
}

// composerBridgeLedger keeps every finalize command, so a test can read how a
// hold was settled.
type composerBridgeLedger struct {
	*rejectionFacadeRepo
	finalized []UpstreamRejectionFinalizeCommand
}

func (l *composerBridgeLedger) FinalizeUpstreamRejection(ctx context.Context, c *UpstreamRejectionFinalizeCommand) (*UpstreamRejectionBillingResult, error) {
	l.finalized = append(l.finalized, *c)
	return l.rejectionFacadeRepo.FinalizeUpstreamRejection(ctx, c)
}

type composerBridgeRun struct {
	ledger     *composerBridgeLedger
	upstream   *httpUpstreamRecorder
	response   *httptest.ResponseRecorder
	result     *OpenAIForwardResult
	forwardErr error
	finishErr  error
}

func (r composerBridgeRun) upstreamPaths() []string {
	paths := make([]string, 0, len(r.upstream.requests))
	for _, request := range r.upstream.requests {
		paths = append(paths, request.URL.Path)
	}
	return paths
}

// runComposerBridgeChat drives one chat request the way the handler does: the
// attempt travels in the request context and is finished afterwards. With held,
// the group charges the rejection fee and the fixed fee is reserved. Without
// it the same account serves a group that does not charge the fee, and the
// handler passes a nil attempt down the same way.
func runComposerBridgeChat(t *testing.T, held bool, model string, body []byte, upstream *httpUpstreamRecorder) composerBridgeRun {
	t.Helper()
	gin.SetMode(gin.TestMode)
	_, repo, key, _ := rejectionFacadeFixture()
	ledger := &composerBridgeLedger{rejectionFacadeRepo: repo}
	account := composerBridgeRejectionAccount()
	require.True(t, account.IsModelSupported(model), "the scheduler may pick this account for %s", model)

	svc := rejectionGatewayService(upstream)
	svc.usageBillingRepo = ledger

	ctx := context.Background()
	var attempt *UpstreamRejectionBillingAttempt
	if held {
		var err error
		attempt, err = svc.BeginUpstreamRejectionBilling(ctx, key, account, model, body)
		require.NoError(t, err)
		require.NotNil(t, attempt, "the group policy reserves the fixed fee for this request")
	}
	ctx = WithUpstreamRejectionBillingAttempt(ctx, attempt)
	c, response := rejectionGatewayContext("/v1/chat/completions", body)
	c.Request = c.Request.WithContext(ctx)
	result, forwardErr := svc.ForwardAsChatCompletions(ctx, c, account, body, "", "")
	finishErr := svc.FinishUpstreamRejectionBillingAttempt(ctx, attempt, result, forwardErr)
	return composerBridgeRun{ledger: ledger, upstream: upstream, response: response, result: result, forwardErr: forwardErr, finishErr: finishErr}
}

// A hold covers one upstream request, and describing images would send more.
// With a hold the composer model therefore takes no image input: the customer
// gets a clear 400, nothing reaches the supplier, and the hold is released at
// once without charge. What the description call would have met upstream makes
// no difference, because it is never sent.
func TestUpstreamRejectionComposerImageInputIsRefusedBeforeAnyUpstreamRequest(t *testing.T) {
	described := func() *httpUpstreamRecorder {
		return &httpUpstreamRecorder{responses: []*http.Response{composerBridgeDescriptionResponse(), composerBridgeDescriptionResponse(), composerBridgeChatResponse(composerBridgeTestModel)}}
	}
	answering := func(status int, body string) func() *httpUpstreamRecorder {
		return func() *httpUpstreamRecorder {
			return &httpUpstreamRecorder{resp: rejectionGatewayResponse(status, "application/json", body)}
		}
	}
	for _, tc := range []struct {
		name     string
		model    string
		images   int
		stream   bool
		upstream func() *httpUpstreamRecorder
	}{
		{"one_image", composerBridgeTestModel, 1, false, described},
		{"two_images", composerBridgeTestModel, 2, false, described},
		{"stream", composerBridgeTestModel, 1, true, described},
		{"model_alias", "grok-composer", 1, false, described},
		{"description_would_get_400", composerBridgeTestModel, 1, false, answering(http.StatusBadRequest, `{"error":{"message":"unsupported image"}}`)},
		{"description_would_get_403_policy", composerBridgeTestModel, 1, false, answering(http.StatusForbidden, `{"error":{"message":"This content violates usage guidelines"}}`)},
		{"description_would_get_500", composerBridgeTestModel, 1, false, answering(http.StatusInternalServerError, `{"error":{"message":"internal error"}}`)},
		{"description_would_fail_to_connect", composerBridgeTestModel, 1, false, func() *httpUpstreamRecorder {
			return &httpUpstreamRecorder{err: errors.New("dial tcp: connection refused")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			images := make([]string, tc.images)
			for i := range images {
				images[i] = composerBridgeTestImage
			}
			run := runComposerBridgeChat(t, true, tc.model, composerBridgeChatRequest(tc.model, tc.stream, images...), tc.upstream())

			require.Empty(t, run.upstreamPaths(), "nothing may reach the supplier")
			require.Equal(t, http.StatusBadRequest, run.response.Code, run.response.Body.String())
			require.Equal(t, "invalid_request_error", gjson.Get(run.response.Body.String(), "error.type").String())
			require.Equal(t, "This model does not support image input in this group", gjson.Get(run.response.Body.String(), "error.message").String())

			require.Nil(t, run.result, "there is no usage to bill")
			require.ErrorIs(t, run.forwardErr, errUpstreamRejectionHeldImageInput)
			require.ErrorIs(t, run.finishErr, errUpstreamRejectionHeldImageInput)
			requireNoRejectionFailover(t, run.finishErr)

			require.Equal(t, 1, run.ledger.reserveCalls)
			require.Zero(t, run.ledger.markCalls, "the hold was never marked as sent")
			require.Equal(t, []UpstreamRejectionFinalizeCommand{{
				RequestID: run.ledger.row.RequestID, APIKeyID: run.ledger.row.APIKeyID,
				Outcome: UpstreamRejectionReleased, ConfirmedNoCharge: true,
			}}, run.ledger.finalized, "released once, as a request that was never sent")
			require.Equal(t, UpstreamRejectionReleased, run.ledger.row.State)
			require.Zero(t, run.ledger.captures, "no fixed fee is charged")
		})
	}
}

// The refusal is limited to image input that needs the bridge. Every other
// request under a hold is still sent, exactly once.
func TestUpstreamRejectionComposerRequestsWithoutBridgedImagesAreSentOnce(t *testing.T) {
	for _, tc := range []struct {
		name       string
		model      string
		imageParts []string
		keepsImage bool
	}{
		{"composer_text_only", composerBridgeTestModel, nil, false},
		{"composer_empty_image", composerBridgeTestModel, []string{`{"type":"image_url","image_url":{"url":"data:image/png;base64,"}}`}, true},
		{"other_model_with_image", rejectionGatewayTestModel, []string{composerBridgeTestImage}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{responses: []*http.Response{composerBridgeChatResponse(tc.model)}}
			run := runComposerBridgeChat(t, true, tc.model, composerBridgeChatRequest(tc.model, false, tc.imageParts...), upstream)

			require.NoError(t, run.finishErr)
			require.Equal(t, []string{"/v1/chat/completions"}, run.upstreamPaths())
			require.Equal(t, tc.keepsImage, strings.Contains(string(upstream.bodies[0]), "image_url"), "the request body is forwarded as the client sent it")
			require.Equal(t, http.StatusOK, run.response.Code, run.response.Body.String())
			require.Equal(t, 1, run.ledger.markCalls)
			require.Equal(t, UpstreamRejectionSent, run.ledger.row.State, "ordinary usage settles the hold afterwards")
			require.Empty(t, run.ledger.finalized)
			require.NotNil(t, run.result)
			require.Equal(t, 3, run.result.Usage.InputTokens)
			require.Equal(t, 5, run.result.Usage.OutputTokens)
		})
	}
}

// A group that does not charge the rejection fee carries no hold, and the
// image bridge works as before on the same account: the description call is
// sent first and its usage is added to the composer request's.
func TestComposerImageBridgeWithoutRejectionHoldDescribesImagesAsBefore(t *testing.T) {
	body := composerBridgeChatRequest(composerBridgeTestModel, false, composerBridgeTestImage)

	t.Run("described", func(t *testing.T) {
		upstream := &httpUpstreamRecorder{responses: []*http.Response{composerBridgeDescriptionResponse(), composerBridgeChatResponse(composerBridgeTestModel)}}
		run := runComposerBridgeChat(t, false, composerBridgeTestModel, body, upstream)

		require.NoError(t, run.forwardErr)
		require.NoError(t, run.finishErr)
		require.Equal(t, []string{"/v1/responses", "/v1/chat/completions"}, run.upstreamPaths())
		require.Equal(t, grokComposerImageBridgeVisionModel, gjson.GetBytes(upstream.bodies[0], "model").String())
		require.NotContains(t, string(upstream.bodies[1]), "image_url")
		require.Contains(t, gjson.GetBytes(upstream.bodies[1], "messages.0.content").String(), "Image 1 description: A small diagram with ABC letters.")
		require.Equal(t, http.StatusOK, run.response.Code, run.response.Body.String())
		require.Equal(t, 14, run.result.Usage.InputTokens)
		require.Equal(t, 12, run.result.Usage.OutputTokens)
		require.Zero(t, run.ledger.reserveCalls+run.ledger.markCalls+run.ledger.finalizeCalls, "no hold is involved")
	})

	for _, tc := range []struct {
		name         string
		upstream     *httpUpstreamRecorder
		wantFailover bool
	}{
		{"description_400", &httpUpstreamRecorder{resp: rejectionGatewayResponse(http.StatusBadRequest, "application/json", `{"error":{"message":"unsupported image"}}`)}, false},
		{"description_500", &httpUpstreamRecorder{resp: rejectionGatewayResponse(http.StatusInternalServerError, "application/json", `{"error":{"message":"internal error"}}`)}, true},
		{"description_connection_failure", &httpUpstreamRecorder{err: io.ErrUnexpectedEOF}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runComposerBridgeChat(t, false, composerBridgeTestModel, body, tc.upstream)

			require.Error(t, run.forwardErr)
			require.Equal(t, run.forwardErr, run.finishErr, "without a hold the error passes through untouched")
			require.NotErrorIs(t, run.forwardErr, errUpstreamRejectionHeldImageInput)
			require.Nil(t, run.result)
			require.Equal(t, []string{"/v1/responses"}, run.upstreamPaths(), "the composer request is not sent without a description")
			var failover *UpstreamFailoverError
			require.Equal(t, tc.wantFailover, errors.As(run.forwardErr, &failover))
			if !tc.wantFailover {
				require.Equal(t, http.StatusBadGateway, run.response.Code, run.response.Body.String())
				require.Equal(t, "upstream_error", gjson.Get(run.response.Body.String(), "error.type").String())
			}
			require.Zero(t, run.ledger.reserveCalls+run.ledger.markCalls+run.ledger.finalizeCalls, "no hold is involved")
		})
	}
}
