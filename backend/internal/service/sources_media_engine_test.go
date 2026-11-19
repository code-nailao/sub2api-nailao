package service

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/stretchr/testify/require"
	"sources.local/mediaengine"
	"strings"
	"testing"
)

func TestSourcesMediaEngineRequestGrammar(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, content, body string
		wantErr                       bool
	}{
		{"default", "/v1/images/generations/async", "application/json", `{"prompt":"test"}`, false},
		{"gpt", "/v1/images/generations/async", "application/json", `{"model":"gpt-image-2","prompt":"test","response_format":"url","size":"1024x1024","quality":"high"}`, false},
		{"gemini_refs", "/v1/images/edits/async", "application/json", `{"model":"gemini-3-pro-image","prompt":"test","reference_images":[{"source":{"image_url":"https://cdn.example/a.png"}}]}`, false},
		{"missing_image", "/v1/images/edits/async", "application/json", `{"prompt":"test"}`, true},
		{"multipart", "/v1/images/edits/async", "multipart/form-data; boundary=x", `{"prompt":"test"}`, true},
		{"duplicate_key", "/v1/images/generations/async", "application/json", `{"prompt":"test","n":1,"n":2}`, true},
		{"mask", "/v1/images/generations/async", "application/json", `{"prompt":"test","mask":"x"}`, true},
		{"stream", "/v1/images/generations/async", "application/json", `{"prompt":"test","stream":true}`, true},
		{"b64", "/v1/images/generations/async", "application/json", `{"prompt":"test","response_format":"b64_json"}`, true},
		{"multiple", "/v1/images/generations/async", "application/json", `{"prompt":"test","n":2}`, true},
		{"resolution_conflict", "/v1/images/generations/async", "application/json", `{"prompt":"test","size":"1024x1024","image_size":"4K"}`, true},
		{"too_many_refs", "/v1/images/generations/async", "application/json", `{"prompt":"test","images":["a","b","c","d","e"]}`, true},
		{"bad_output_type", "/v1/images/generations/async", "application/json", `{"prompt":"test","output_format":3}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := parseSourcesMediaEngineRequest(tc.endpoint, tc.content, []byte(tc.body))
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Len(t, r.Hash, 64)
			}
		})
	}
}
func TestSourcesMediaEngineIdentityHashIndependentOfFormatting(t *testing.T) {
	a, err := parseSourcesMediaEngineRequest("/v1/images/generations/async", "application/json", []byte(`{"prompt":"test","model":"gpt-image-2"}`))
	require.NoError(t, err)
	b, err := parseSourcesMediaEngineRequest("/v1/images/generations/async", "application/json", []byte(`{ "model" : "gpt-image-2", "prompt" : "test" }`))
	require.NoError(t, err)
	require.Equal(t, a.Hash, b.Hash)
	model, body, err := SourcesMediaEngineAuditInput("/v1/images/edits/async", "application/json", []byte(`{"prompt":"test","image":"https://cdn.example/one","reference_images":["https://cdn.example/two"]}`))
	require.NoError(t, err)
	require.Equal(t, "gpt-image-2", model)
	input := ExtractContentModerationInput(ContentModerationProtocolOpenAIImages, body)
	encoded, _ := json.Marshal(input)
	require.Contains(t, string(encoded), "cdn.example/one")
	require.Contains(t, string(encoded), "cdn.example/two")
}
func TestSourcesMediaEngineTicketRejectsTamperedDimensions(t *testing.T) {
	owner := mediaengine.Owner{UserID: 1, KeyID: 2, GroupID: 74}
	group := int64(74)
	ticket := sourcesMediaEngineTicket{Version: 1, Owner: owner, Platform: PlatformOpenAI, AccountID: 713, Amount: .03, Billing: UsageBillingCommand{UserID: 1, APIKeyID: 2, AccountID: 713, AccountType: AccountTypeAPIKey, Model: "gpt-image-2", ImageCount: 1, BillingType: BillingTypeBalance, RequestPayloadHash: strings.Repeat("a", 64)}, Log: UsageLog{UserID: 1, APIKeyID: 2, AccountID: 713, GroupID: &group, Model: "gpt-image-2", ImageCount: 1, BillingType: BillingTypeBalance, ActualCost: .03, TotalCost: .03, RateMultiplier: 1}}
	ticket.Billing.Normalize()
	raw, err := json.Marshal(ticket)
	require.NoError(t, err)
	task := &mediaengine.Task{Owner: owner, AccountID: 713, PayloadHash: strings.Repeat("a", 64), Ticket: raw}
	_, err = sourcesImageTicket(task)
	require.NoError(t, err)
	ticket.Log.ActualCost = .3
	task.Ticket, _ = json.Marshal(ticket)
	_, err = sourcesImageTicket(task)
	require.ErrorIs(t, err, mediaengine.ErrConflict)
}
func TestSourcesMediaEngineNotConfiguredLeavesOrdinaryEngineUntouched(t *testing.T) {
	t.Setenv("SOURCES_IMAGE_ENGINES_FILE", "")
	s, err := NewSourcesMediaEngine(nil, nil, nil, nil)
	require.NoError(t, err)
	require.Nil(t, s)
	handled, err := s.Handles(&APIKey{}, "key")
	require.False(t, handled)
	require.NoError(t, err)
	s.Stop()
}
func TestSourcesMediaEngineBillingErrorPreservesNativeCause(t *testing.T) {
	err := &SourcesImageBillingError{Cause: ErrBillingServiceUnavailable}
	require.True(t, errors.Is(err, ErrBillingServiceUnavailable))
	_ = context.Background()
}
