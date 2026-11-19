//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// deepSeekToolsTestGroup is a DeepSeek standard group with all five tools
// priced. The group rate is deliberately not 1: tool prices never use it.
const deepSeekToolsTestGroup = int64(7)

type deepSeekToolsTestPrices struct{ search, fetch, ocr, asr, tts *float64 }

func deepSeekPrice(v float64) *float64 { return &v }

func deepSeekToolsAllPrices() deepSeekToolsTestPrices {
	return deepSeekToolsTestPrices{search: deepSeekPrice(6), fetch: deepSeekPrice(6), ocr: deepSeekPrice(3), asr: deepSeekPrice(0.36), tts: deepSeekPrice(2)}
}

func deepSeekToolsTestKey(prices deepSeekToolsTestPrices, balance float64) *APIKey {
	groupID := deepSeekToolsTestGroup
	return &APIKey{ID: 2, UserID: 3, GroupID: &groupID, User: &User{ID: 3, Balance: balance}, Group: &Group{
		ID: groupID, Platform: PlatformDeepseek, SubscriptionType: SubscriptionTypeStandard, RateMultiplier: 0.35, SearchPricePer1k: prices.search,
		DeepSeekTools: DeepSeekToolsConfig{Enabled: true, FetchPricePer1K: prices.fetch, OCRPricePer1KPages: prices.ocr, ASRPricePerHour: prices.asr, TTSPricePer10KChars: prices.tts},
	}}
}

func deepSeekToolsTestAccount(id int64, tools bool) Account {
	extra := map[string]any{}
	if tools {
		extra["deepseek_tools_enabled"] = true
	}
	return Account{ID: id, Name: fmt.Sprintf("tools-%d", id), Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 2,
		GroupIDs: []int64{deepSeekToolsTestGroup}, Credentials: map[string]any{"api_key": fmt.Sprintf("fixture-key-%d", id), "base_url": "https://provider.example/v1"}, Extra: extra}
}

type deepSeekToolsJournal struct {
	deepSeekJournalFixture
	mu     sync.Mutex
	begins []int64 // account of each journal entry
}

func (j *deepSeekToolsJournal) BeginDeepSeekUsage(_ context.Context, requestID string, _, _, accountID int64, deadline time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !strings.HasPrefix(requestID, "deepseek-tools:") || !deadline.After(time.Now()) {
		return errors.New("fixture: bad journal entry")
	}
	j.begins = append(j.begins, accountID)
	return nil
}

// settled returns the final (ready) checkpoint, or nil when nothing was settled.
func (j *deepSeekToolsJournal) settled(t *testing.T) *DeepSeekUsageCheckpoint {
	t.Helper()
	var ready []*DeepSeekUsageCheckpoint
	for _, c := range j.checkpoints {
		if c.Ready {
			ready = append(ready, c)
		}
	}
	require.LessOrEqual(t, len(ready), 1, "a standalone tool settles at most once")
	if len(ready) == 0 {
		return nil
	}
	return ready[0]
}

// deepSeekToolsBillingCache adds the cache invalidations settlement performs.
type deepSeekToolsBillingCache struct{ *memInflightCache }

func (deepSeekToolsBillingCache) InvalidateUserBalance(context.Context, int64) error     { return nil }
func (deepSeekToolsBillingCache) InvalidateAPIKeyRateLimit(context.Context, int64) error { return nil }

type deepSeekToolsCall struct {
	account int64
	path    string
	body    []byte
	ctx     context.Context
}

type deepSeekToolsHarness struct {
	svc     *OpenAIGatewayService
	journal *deepSeekToolsJournal
	cache   *memInflightCache
	mu      sync.Mutex
	calls   []deepSeekToolsCall
}

func (h *deepSeekToolsHarness) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func newDeepSeekToolsHarness(t *testing.T, accounts []Account, upstream func(call deepSeekToolsCall) (*http.Response, error)) *deepSeekToolsHarness {
	t.Helper()
	cfg := deepSeekChatFallbackTestConfig()
	cfg.Billing.InflightReservation.Enabled = true
	h := &deepSeekToolsHarness{journal: &deepSeekToolsJournal{}, cache: newMemInflightCache(10)}
	http := deepSeekHTTPFixture{fn: func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var account int64
		_, _ = fmt.Sscanf(r.Header.Get("Authorization"), "Bearer fixture-key-%d", &account)
		call := deepSeekToolsCall{account: account, path: r.URL.Path, body: body, ctx: r.Context()}
		h.mu.Lock()
		h.calls = append(h.calls, call)
		h.mu.Unlock()
		return upstream(call)
	}}
	h.svc = &OpenAIGatewayService{cfg: cfg, httpUpstream: http, accountRepo: stubOpenAIAccountRepo{accounts: accounts}, usageBillingRepo: h.journal,
		billingService: newTestBillingService(), billingCacheService: &BillingCacheService{cfg: cfg, cache: deepSeekToolsBillingCache{h.cache}}}
	return h
}

func (h *deepSeekToolsHarness) serve(t *testing.T, ctx context.Context, w http.ResponseWriter, key *APIKey, operation, contentType string, body []byte) {
	t.Helper()
	req, err := PrepareDeepSeekToolRequest(operation, contentType, body)
	require.NoError(t, err)
	require.NoError(t, h.svc.ServeDeepSeekTool(ctx, w, key, req, OpenAIRecordUsageInput{InboundEndpoint: "/v1/tools/" + operation, PricingAt: time.Now()}))
}

// requireReservationReleased checks that the one reservation of the request
// was released exactly once.
func (h *deepSeekToolsHarness) requireReservationReleased(t *testing.T) {
	t.Helper()
	require.Zero(t, h.cache.count(), "no reservation may outlive the request")
	require.Equal(t, int32(1), h.cache.releaseCt.Load(), "the reservation is released exactly once")
}

func deepSeekJSONResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func deepSeekMultipart(t *testing.T, fields map[string]string, fileName string, file []byte) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		require.NoError(t, w.WriteField(k, v))
	}
	if file != nil {
		part, err := w.CreateFormFile("file", fileName)
		require.NoError(t, err)
		_, err = part.Write(file)
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return w.FormDataContentType(), buf.Bytes()
}

// deepSeekWAV is a 16 kHz mono 16-bit PCM WAV of the given length.
func deepSeekWAV(seconds float64) []byte {
	const rate, channels, bits = 16000, 1, 16
	data := make([]byte, int(seconds*rate)*channels*bits/8)
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	for _, field := range []any{uint32(16), uint16(1), uint16(channels), uint32(rate), uint32(rate * channels * bits / 8), uint16(channels * bits / 8), uint16(bits)} {
		_ = binary.Write(&b, binary.LittleEndian, field)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

// deepSeekOggOpus has an OpusHead page and a final page at the given granule.
func deepSeekOggOpus(seconds float64) []byte {
	page := func(granule uint64, payload []byte) []byte {
		var b bytes.Buffer
		b.WriteString("OggS")
		b.Write([]byte{0, 0})
		_ = binary.Write(&b, binary.LittleEndian, granule)
		b.Write(make([]byte, 12))
		b.Write([]byte{1, byte(len(payload))})
		b.Write(payload)
		return b.Bytes()
	}
	head := append([]byte("OpusHead\x01\x01"), 0x38, 0x01) // pre-skip 312
	head = append(head, make([]byte, 7)...)
	return append(page(0, head), page(uint64(seconds*48000)+312, []byte("audio"))...)
}

func TestDeepSeekStandaloneSearchKeepsItsBillingAndShape(t *testing.T) {
	h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true)}, func(call deepSeekToolsCall) (*http.Response, error) {
		require.Equal(t, "/v1/tools/search", call.path)
		require.JSONEq(t, `{"query":"Python","count":3}`, string(call.body))
		return deepSeekJSONResponse(200, `{"results":[{"title":"A","url":"https://example.com/a"}]}`), nil
	})
	key := deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10)
	rec := httptest.NewRecorder()
	h.serve(t, context.Background(), rec, key, DeepSeekToolSearch, "application/json", []byte(`{"query":"Python","count":3}`))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "https://example.com/a")
	require.True(t, strings.HasPrefix(rec.Header().Get("X-Sources-Request-Id"), "deepseek-tools:"))
	settled := h.journal.settled(t)
	require.NotNil(t, settled)
	require.InDelta(t, 0.006, settled.Command.BalanceCost, 1e-12, "one search at 6 per 1,000, no group rate")
	// A Search receipt and its fingerprint keep the shape they had before the
	// document and speech tools existed.
	wire, err := json.Marshal(settled.Usage.BillingSnapshot.DeepSeekTools)
	require.NoError(t, err)
	require.JSONEq(t, `{"search_count":1,"fetch_count":0,"search_price_per_call":0.006,"fetch_price_per_call":0.006,"supplier_account_id":11,"supplier_search_price_per_call":0,"supplier_fetch_price_per_call":0,"standalone":true}`, string(wire))
	require.Equal(t, "1:0:0.00600000:0.00600000:11:0.00000000:0.00000000", settled.Command.DeepSeekToolFingerprint)
	h.requireReservationReleased(t)
}

func TestDeepSeekStandaloneFetchWithoutContentIsNotCharged(t *testing.T) {
	h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true)}, func(deepSeekToolsCall) (*http.Response, error) {
		return deepSeekJSONResponse(200, `{"content":"   "}`), nil
	})
	rec := httptest.NewRecorder()
	h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolFetch, "", []byte(`{"url":"https://example.com/page"}`))
	require.Equal(t, http.StatusOK, rec.Code)
	settled := h.journal.settled(t)
	require.Zero(t, settled.Command.BalanceCost)
	require.Zero(t, settled.Usage.BillingSnapshot.DeepSeekTools.FetchCount)
	h.requireReservationReleased(t)
}

func TestDeepSeekOCRForwardsHeartbeatsAndChargesConfirmedPages(t *testing.T) {
	writes := make(chan struct{})
	writer := &deepSeekSignalWriter{ResponseRecorder: httptest.NewRecorder(), first: writes}
	h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true)}, func(call deepSeekToolsCall) (*http.Response, error) {
		require.Equal(t, "/v1/tools/ocr", call.path)
		require.Contains(t, string(call.body), "text_recognition")
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte("  \n"))
			select {
			case <-writes: // the heartbeat reached the client before the result exists
			case <-time.After(5 * time.Second):
				_ = pw.CloseWithError(errors.New("heartbeat was not forwarded"))
				return
			}
			_, _ = pw.Write([]byte(" \n{\"pages\":[{\"text\":\"one\"},{\"text\":\"two\"},{\"text\":\"\"}],\"page_count\":3}"))
			_ = pw.Close()
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: pr}, nil
	})
	contentType, body := deepSeekMultipart(t, map[string]string{"purpose": "text_recognition"}, "scan.pdf", []byte("%PDF-1.7\n1 0 obj << /Type /Page >> endobj"))
	h.serve(t, context.Background(), writer, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolOCR, contentType, body)

	require.Equal(t, http.StatusOK, writer.Code)
	require.True(t, strings.HasPrefix(writer.Body.String(), "  \n \n{"), writer.Body.String())
	require.Equal(t, int64(3), gjson.Get(strings.TrimSpace(writer.Body.String()), "page_count").Int())
	settled := h.journal.settled(t)
	require.Equal(t, 3, settled.Usage.BillingSnapshot.DeepSeekTools.OCRPages)
	require.InDelta(t, 0.009, settled.Command.BalanceCost, 1e-12, "3 pages at 3 per 1,000 pages")
	require.InDelta(t, 0.009, settled.Usage.ActualCost, 1e-12)
	h.requireReservationReleased(t)
}

// deepSeekSignalWriter closes first on the first body write.
type deepSeekSignalWriter struct {
	*httptest.ResponseRecorder
	first chan struct{}
	once  sync.Once
	gone  atomic.Bool
}

func (w *deepSeekSignalWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.first) })
	if w.gone.Load() {
		return 0, errors.New("client gone")
	}
	return w.ResponseRecorder.Write(p)
}

func TestDeepSeekOCRFinishesAndChargesAfterClientDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	writes := make(chan struct{})
	writer := &deepSeekSignalWriter{ResponseRecorder: httptest.NewRecorder(), first: writes}
	h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true)}, func(call deepSeekToolsCall) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte("\n"))
			<-writes
			writer.gone.Store(true)
			cancel() // the client leaves while the supplier is still working
			if call.ctx.Err() != nil {
				_ = pw.CloseWithError(errors.New("supplier call was cancelled with the client"))
				return
			}
			_, _ = pw.Write([]byte(`{"pages":[{"text":"x"}],"page_count":1}`))
			_ = pw.Close()
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	})
	contentType, body := deepSeekMultipart(t, map[string]string{"purpose": "text_recognition"}, "page.png", []byte("\x89PNG\r\n\x1a\nimage"))
	h.serve(t, ctx, writer, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolOCR, contentType, body)
	settled := h.journal.settled(t)
	require.NotNil(t, settled, "confirmed work is billed after a disconnect")
	require.Equal(t, 1, settled.Usage.BillingSnapshot.DeepSeekTools.OCRPages)
	h.requireReservationReleased(t)
}

func TestDeepSeekOCRResultsThatAreNotCharged(t *testing.T) {
	for name, answer := range map[string]string{
		"no_pages":       `{"pages":[],"page_count":0}`,
		"count_missing":  `{"pages":[{"text":"x"}]}`,
		"error_envelope": `{"error":{"message":"document could not be read"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true)}, func(deepSeekToolsCall) (*http.Response, error) {
				return deepSeekJSONResponse(200, answer), nil
			})
			rec := httptest.NewRecorder()
			contentType, body := deepSeekMultipart(t, map[string]string{"purpose": "text_recognition"}, "a.png", []byte("\x89PNG\r\n\x1a\nimage"))
			h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolOCR, contentType, body)
			require.Equal(t, http.StatusOK, rec.Code)
			require.JSONEq(t, answer, rec.Body.String())
			require.Zero(t, h.journal.settled(t).Command.BalanceCost)
			h.requireReservationReleased(t)
		})
	}
}

func TestDeepSeekASRChargesStartedSecondsPerHour(t *testing.T) {
	for _, tc := range []struct {
		durationMS float64
		seconds    int
	}{{61001, 62}, {1, 1}, {60000, 60}} {
		t.Run(fmt.Sprint(tc.durationMS), func(t *testing.T) {
			h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true)}, func(call deepSeekToolsCall) (*http.Response, error) {
				require.Equal(t, "/v1/tools/asr", call.path)
				return deepSeekJSONResponse(200, fmt.Sprintf(`{"text":"hello","duration_ms":%v}`, tc.durationMS)), nil
			})
			rec := httptest.NewRecorder()
			contentType, body := deepSeekMultipart(t, nil, "speech.wav", deepSeekWAV(2))
			h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolASR, contentType, body)
			require.Equal(t, http.StatusOK, rec.Code)
			settled := h.journal.settled(t)
			require.Equal(t, tc.seconds, settled.Usage.BillingSnapshot.DeepSeekTools.ASRSeconds)
			require.InDelta(t, float64(tc.seconds)*0.36/3600, settled.Command.BalanceCost, 1e-12)
			h.requireReservationReleased(t)
		})
	}
}

func TestDeepSeekASRWithoutTextIsNotCharged(t *testing.T) {
	h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true)}, func(deepSeekToolsCall) (*http.Response, error) {
		return deepSeekJSONResponse(200, `{"text":"","duration_ms":3000}`), nil
	})
	rec := httptest.NewRecorder()
	contentType, body := deepSeekMultipart(t, nil, "speech.ogg", deepSeekOggOpus(3))
	h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolASR, contentType, body)
	require.Zero(t, h.journal.settled(t).Command.BalanceCost)
	h.requireReservationReleased(t)
}

func TestDeepSeekTTSStreamsAudioAndCountsHanCharactersTwice(t *testing.T) {
	audio := bytes.Repeat([]byte("RIFFaudio"), 10000)
	h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true)}, func(call deepSeekToolsCall) (*http.Response, error) {
		require.Equal(t, "/v1/tools/tts", call.path)
		require.JSONEq(t, `{"text":"你好, ab","voice_id":"voice-a","format":"wav"}`, string(call.body))
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"audio/wav"}}, Body: io.NopCloser(bytes.NewReader(audio))}, nil
	})
	rec := httptest.NewRecorder()
	h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolTTS, "application/json", []byte(`{"text":"你好, ab","voice_id":"voice-a","format":"wav"}`))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "audio/wav", rec.Header().Get("Content-Type"))
	require.Equal(t, audio, rec.Body.Bytes())
	settled := h.journal.settled(t)
	// 2 Han characters count 2 each; ",", " ", "a", "b" count 1 each.
	require.Equal(t, 8, settled.Usage.BillingSnapshot.DeepSeekTools.TTSCharacters)
	require.InDelta(t, 8*2.0/10000, settled.Command.BalanceCost, 1e-12)
	h.requireReservationReleased(t)
}

func TestDeepSeekTTSAnswersThatAreNotCharged(t *testing.T) {
	for name, respond := range map[string]func() *http.Response{
		"json_error_envelope": func() *http.Response {
			return deepSeekJSONResponse(200, `{"error":{"message":"voice not found"}}`)
		},
		"audio_cut_short": func() *http.Response {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"audio/wav"}}, Body: io.NopCloser(io.MultiReader(strings.NewReader("RIFFpartial"), iotestErrReader{}))}
		},
		"no_audio": func() *http.Response {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"audio/wav"}}, Body: io.NopCloser(strings.NewReader(""))}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true)}, func(deepSeekToolsCall) (*http.Response, error) { return respond(), nil })
			rec := httptest.NewRecorder()
			h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolTTS, "application/json", []byte(`{"text":"hello"}`))
			if name == "json_error_envelope" {
				// The supplier's answer is passed on and recorded as a free row.
				require.Equal(t, http.StatusOK, rec.Code)
				require.Contains(t, rec.Body.String(), "voice not found")
				require.Zero(t, h.journal.settled(t).Command.BalanceCost)
			} else {
				require.Nil(t, h.journal.settled(t))
			}
			h.requireReservationReleased(t)
		})
	}
}

type iotestErrReader struct{}

func (iotestErrReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestDeepSeekTTSStopsTheSupplierWhenTheClientLeaves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	writes := make(chan struct{})
	writer := &deepSeekSignalWriter{ResponseRecorder: httptest.NewRecorder(), first: writes}
	var supplierCtx context.Context
	h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true)}, func(call deepSeekToolsCall) (*http.Response, error) {
		supplierCtx = call.ctx
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte("RIFFfirst"))
			<-writes
			writer.gone.Store(true)
			cancel()
			<-call.ctx.Done()
			_ = pw.CloseWithError(call.ctx.Err())
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"audio/wav"}}, Body: pr}, nil
	})
	h.serve(t, ctx, writer, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolTTS, "application/json", []byte(`{"text":"hello"}`))
	require.Error(t, supplierCtx.Err(), "the supplier call ends with the client")
	settled := h.journal.settled(t)
	require.NotNil(t, settled, "synthesis had started: the input is charged")
	require.Equal(t, 5, settled.Usage.BillingSnapshot.DeepSeekTools.TTSCharacters)
	h.requireReservationReleased(t)
}

func TestDeepSeekToolsFailOverOnlyOnDefinitiveRejection(t *testing.T) {
	accounts := []Account{deepSeekToolsTestAccount(11, true), deepSeekToolsTestAccount(12, true), deepSeekToolsTestAccount(13, false)}
	first := map[int64]int{}
	h := newDeepSeekToolsHarness(t, accounts, func(call deepSeekToolsCall) (*http.Response, error) {
		require.NotEqual(t, int64(13), call.account, "an account without tools is never used")
		first[call.account]++
		if len(first) == 1 {
			return deepSeekJSONResponse(http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`), nil
		}
		return deepSeekJSONResponse(200, `{"text":"hello","duration_ms":1500}`), nil
	})
	rec := httptest.NewRecorder()
	contentType, body := deepSeekMultipart(t, nil, "a.wav", deepSeekWAV(1.5))
	h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolASR, contentType, body)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, first, 2, "the second tool account served the request")
	for _, n := range first {
		require.Equal(t, 1, n, "no account is asked twice")
	}
	settled := h.journal.settled(t)
	require.Equal(t, 2, settled.Usage.BillingSnapshot.DeepSeekTools.ASRSeconds)
	require.Equal(t, h.calls[1].account, settled.Usage.BillingSnapshot.DeepSeekTools.SupplierAccountID)
	require.Equal(t, h.calls[1].account, settled.Usage.AccountID)
	require.Len(t, h.journal.begins, 2, "each attempt has its own journal entry; the refused one settles as nothing")
	h.requireReservationReleased(t)
}

func TestDeepSeekToolsNeverReplayAnUnknownOutcome(t *testing.T) {
	for name, respond := range map[string]func() (*http.Response, error){
		"transport_failure": func() (*http.Response, error) { return nil, errors.New("connection reset after send") },
		"gateway_timeout":   func() (*http.Response, error) { return deepSeekJSONResponse(http.StatusGatewayTimeout, `{}`), nil },
		"bad_gateway":       func() (*http.Response, error) { return deepSeekJSONResponse(http.StatusBadGateway, `{}`), nil },
	} {
		t.Run(name, func(t *testing.T) {
			h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true), deepSeekToolsTestAccount(12, true)}, func(deepSeekToolsCall) (*http.Response, error) { return respond() })
			rec := httptest.NewRecorder()
			h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolTTS, "", []byte(`{"text":"hello"}`))
			require.Equal(t, 1, h.callCount(), "a request that may have been processed is not sent again")
			require.Equal(t, http.StatusBadGateway, rec.Code)
			require.Equal(t, "false", rec.Header().Get("x-should-retry"))
			require.Nil(t, h.journal.settled(t), "an unknown outcome is not charged")
			h.requireReservationReleased(t)
		})
	}
}

func TestDeepSeekToolsSupplierInputRejectionIsNotFailedOver(t *testing.T) {
	h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true), deepSeekToolsTestAccount(12, true)}, func(deepSeekToolsCall) (*http.Response, error) {
		return deepSeekJSONResponse(http.StatusBadRequest, `{"error":{"message":"document has too many pages"}}`), nil
	})
	rec := httptest.NewRecorder()
	contentType, body := deepSeekMultipart(t, map[string]string{"purpose": "text_recognition"}, "a.pdf", []byte("%PDF-1.4 /Type /Pages"))
	h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolOCR, contentType, body)
	require.Equal(t, 1, h.callCount())
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "document has too many pages")
	require.Nil(t, h.journal.settled(t))
	h.requireReservationReleased(t)
}

func TestDeepSeekToolsAllAccountsRefusing(t *testing.T) {
	h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true), deepSeekToolsTestAccount(12, true)}, func(deepSeekToolsCall) (*http.Response, error) {
		return deepSeekJSONResponse(http.StatusPaymentRequired, `{"error":{"message":"insufficient balance"}}`), nil
	})
	rec := httptest.NewRecorder()
	h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolSearch, "", []byte(`{"query":"x"}`))
	require.Equal(t, 2, h.callCount())
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Nil(t, h.journal.settled(t))
	h.requireReservationReleased(t)
}

func TestDeepSeekToolsPinnedAccountOverridesScheduling(t *testing.T) {
	h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true), deepSeekToolsTestAccount(12, true)}, func(call deepSeekToolsCall) (*http.Response, error) {
		require.Equal(t, int64(12), call.account)
		return deepSeekJSONResponse(200, `{"content":"page"}`), nil
	})
	key := deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10)
	pinned := int64(12)
	key.Group.DeepSeekTools.AccountID = &pinned
	rec := httptest.NewRecorder()
	h.serve(t, context.Background(), rec, key, DeepSeekToolFetch, "", []byte(`{"url":"https://example.com/"}`))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, h.journal.settled(t).Usage.BillingSnapshot.DeepSeekTools.FetchCount)
}

func TestDeepSeekToolsRefuseBeforeAnySupplierCall(t *testing.T) {
	never := func(deepSeekToolsCall) (*http.Response, error) {
		t.Fatal("no supplier call may be made")
		return nil, nil
	}
	t.Run("tool_not_offered", func(t *testing.T) {
		prices := deepSeekToolsAllPrices()
		prices.ocr = nil
		h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true)}, never)
		rec := httptest.NewRecorder()
		contentType, body := deepSeekMultipart(t, map[string]string{"purpose": "text_recognition"}, "a.png", []byte("\x89PNG\r\n\x1a\nx"))
		h.serve(t, context.Background(), rec, deepSeekToolsTestKey(prices, 10), DeepSeekToolOCR, contentType, body)
		require.Equal(t, http.StatusForbidden, rec.Code)
	})
	t.Run("insufficient_balance_for_the_estimate", func(t *testing.T) {
		h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true)}, never)
		rec := httptest.NewRecorder()
		// 10,000 characters at 2 per 10,000 need 2; the user has 1.
		h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 1), DeepSeekToolTTS, "", []byte(`{"text":"`+strings.Repeat("a", 10000)+`"}`))
		require.Equal(t, http.StatusPaymentRequired, rec.Code)
	})
	t.Run("no_tool_account", func(t *testing.T) {
		h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, false)}, never)
		rec := httptest.NewRecorder()
		h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolSearch, "", []byte(`{"query":"x"}`))
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		h.requireReservationReleased(t)
	})
}

func TestDeepSeekToolEstimatesAreReserved(t *testing.T) {
	group := deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10).Group
	for _, tc := range []struct {
		op    string
		units int
		want  float64
	}{
		{DeepSeekToolSearch, 1, 0.006},
		{DeepSeekToolASR, 120, 120 * 0.36 / 3600},
		{DeepSeekToolTTS, 30, 30 * 2.0 / 10000},
		{DeepSeekToolOCR, 7, 7 * 3.0 / 1000},
	} {
		got, offered := DeepSeekToolEstimate(group, &DeepSeekToolRequest{Operation: tc.op, EstimateUnits: tc.units})
		require.True(t, offered)
		require.InDelta(t, tc.want, got, 1e-12, tc.op)
	}
	// Free is a price, not "not offered".
	group.DeepSeekTools.TTSPricePer10KChars = deepSeekPrice(0)
	got, offered := DeepSeekToolEstimate(group, &DeepSeekToolRequest{Operation: DeepSeekToolTTS, EstimateUnits: 30})
	require.True(t, offered)
	require.Zero(t, got)
}

func TestDeepSeekToolSupplierCostsAreOptional(t *testing.T) {
	account := deepSeekToolsTestAccount(11, true)
	require.NoError(t, ValidateDeepSeekToolAccountConfig(PlatformDeepseek, AccountTypeAPIKey, account.Extra))
	require.NoError(t, ValidateDeepSeekToolAccountConfig(PlatformDeepseek, AccountTypeAPIKey, map[string]any{"deepseek_tools_enabled": true, "deepseek_search_cost_per_call": nil}))
	require.Error(t, ValidateDeepSeekToolAccountConfig(PlatformDeepseek, AccountTypeAPIKey, map[string]any{"deepseek_tools_enabled": true, "deepseek_ocr_cost_per_1k_pages": -1.0}))
	require.Error(t, ValidateDeepSeekToolAccountConfig(PlatformDeepseek, AccountTypeAPIKey, map[string]any{"deepseek_tools_enabled": true, "deepseek_tts_cost_per_10k_chars": "1"}))

	account.Extra["deepseek_ocr_cost_per_1k_pages"] = 1.5
	key := deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10)
	u, err := deepSeekStandaloneSnapshot(key, &account, DeepSeekToolOCR)
	require.NoError(t, err)
	u.OCRPages = 4
	require.InDelta(t, 0.012, u.Cost(), 1e-12)
	require.InDelta(t, 0.006, u.SupplierCost(), 1e-12)
	require.Zero(t, u.SupplierSearchPrice, "a missing supplier cost counts as 0")
}

func TestDeepSeekToolsConfigNewPrices(t *testing.T) {
	price := 6.0
	base := DeepSeekToolsConfig{Enabled: true, FetchPricePer1K: &price}
	require.NoError(t, ValidateDeepSeekToolsConfig(PlatformDeepseek, SubscriptionTypeStandard, base, &price), "enabling only Search and Fetch stays valid")
	for _, set := range []func(*DeepSeekToolsConfig, *float64){
		func(c *DeepSeekToolsConfig, v *float64) { c.OCRPricePer1KPages = v },
		func(c *DeepSeekToolsConfig, v *float64) { c.ASRPricePerHour = v },
		func(c *DeepSeekToolsConfig, v *float64) { c.TTSPricePer10KChars = v },
	} {
		cfg := cloneDeepSeekToolsConfig(base)
		set(&cfg, deepSeekPrice(-1))
		require.Error(t, ValidateDeepSeekToolsConfig(PlatformDeepseek, SubscriptionTypeStandard, cfg, &price))
		set(&cfg, deepSeekPrice(0))
		require.NoError(t, ValidateDeepSeekToolsConfig(PlatformDeepseek, SubscriptionTypeStandard, cfg, &price))
	}
	cfg := base
	cfg.ASRPricePerHour = deepSeekPrice(1)
	clone := cloneDeepSeekToolsConfig(cfg)
	*clone.ASRPricePerHour = 9
	require.Equal(t, 1.0, *cfg.ASRPricePerHour)
	wire, err := json.Marshal(base)
	require.NoError(t, err)
	require.JSONEq(t, `{"enabled":true,"auto_inject":false,"fetch_price_per_1k":6}`, string(wire), "existing groups store the same JSON")
}

func TestDeepSeekToolRequestValidation(t *testing.T) {
	wav := deepSeekWAV(1)
	for _, tc := range []struct {
		name, op, contentType string
		body                  func() []byte
		status                int
	}{
		{"tts_unknown_field", DeepSeekToolTTS, "application/json", func() []byte { return []byte(`{"text":"hi","model":"x"}`) }, 400},
		{"tts_bad_format", DeepSeekToolTTS, "", func() []byte { return []byte(`{"text":"hi","format":"mp3"}`) }, 400},
		{"tts_blank", DeepSeekToolTTS, "", func() []byte { return []byte(`{"text":"  "}`) }, 400},
		{"tts_too_many_characters", DeepSeekToolTTS, "", func() []byte { return []byte(`{"text":"` + strings.Repeat("字", 5001) + `"}`) }, 400},
		{"tts_body_too_large", DeepSeekToolTTS, "", func() []byte { return bytes.Repeat([]byte(" "), 64<<10+1) }, 413},
		{"asr_not_multipart", DeepSeekToolASR, "application/json", func() []byte { return []byte(`{}`) }, 415},
		{"asr_no_file", DeepSeekToolASR, "", nil, 400},
		{"asr_extra_field", DeepSeekToolASR, "", nil, 400},
		{"asr_mp3", DeepSeekToolASR, "", nil, 400},
		{"asr_too_long", DeepSeekToolASR, "", nil, 400},
		{"asr_8bit_wav", DeepSeekToolASR, "", nil, 400},
		{"asr_ogg_too_long", DeepSeekToolASR, "", nil, 400},
		{"asr_file_too_large", DeepSeekToolASR, "", nil, 413},
		{"ocr_missing_purpose", DeepSeekToolOCR, "", nil, 400},
		{"ocr_wrong_purpose", DeepSeekToolOCR, "", nil, 400},
		{"ocr_unsupported_type", DeepSeekToolOCR, "", nil, 400},
		{"ocr_two_files", DeepSeekToolOCR, "", nil, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contentType, body := tc.contentType, []byte(nil)
			if tc.body != nil {
				body = tc.body()
			}
			switch tc.name {
			case "asr_no_file":
				contentType, body = deepSeekMultipart(t, nil, "", nil)
			case "asr_extra_field":
				contentType, body = deepSeekMultipart(t, map[string]string{"model": "x"}, "a.wav", wav)
			case "asr_mp3":
				contentType, body = deepSeekMultipart(t, nil, "a.mp3", []byte("ID3\x03audio"))
			case "asr_too_long":
				contentType, body = deepSeekMultipart(t, nil, "a.wav", deepSeekWAV(121))
			case "asr_8bit_wav":
				eight := deepSeekWAV(1)
				binary.LittleEndian.PutUint16(eight[34:], 8)
				contentType, body = deepSeekMultipart(t, nil, "a.wav", eight)
			case "asr_ogg_too_long":
				contentType, body = deepSeekMultipart(t, nil, "a.ogg", deepSeekOggOpus(121))
			case "asr_file_too_large":
				contentType, body = deepSeekMultipart(t, nil, "a.wav", append(deepSeekWAV(1), make([]byte, 8<<20)...))
			case "ocr_missing_purpose":
				contentType, body = deepSeekMultipart(t, nil, "a.png", []byte("\x89PNG\r\n\x1a\nx"))
			case "ocr_wrong_purpose":
				contentType, body = deepSeekMultipart(t, map[string]string{"purpose": "chat"}, "a.png", []byte("\x89PNG\r\n\x1a\nx"))
			case "ocr_unsupported_type":
				contentType, body = deepSeekMultipart(t, map[string]string{"purpose": "text_recognition"}, "a.exe", []byte("MZ\x90\x00"))
			case "ocr_two_files":
				var buf bytes.Buffer
				w := multipart.NewWriter(&buf)
				require.NoError(t, w.WriteField("purpose", "text_recognition"))
				for i := 0; i < 2; i++ {
					part, err := w.CreateFormFile("file", "a.png")
					require.NoError(t, err)
					_, _ = part.Write([]byte("\x89PNG\r\n\x1a\nx"))
				}
				require.NoError(t, w.Close())
				contentType, body = w.FormDataContentType(), buf.Bytes()
			}
			_, err := PrepareDeepSeekToolRequest(tc.op, contentType, body)
			var input *DeepSeekToolInputError
			require.ErrorAs(t, err, &input)
			require.Equal(t, tc.status, input.Status, err.Error())
		})
	}
}

func TestDeepSeekToolRequestAcceptsDocumentedInputs(t *testing.T) {
	contentType, body := deepSeekMultipart(t, nil, "a.wav", deepSeekWAV(120))
	req, err := PrepareDeepSeekToolRequest(DeepSeekToolASR, contentType, body)
	require.NoError(t, err)
	require.Equal(t, 120, req.EstimateUnits, "ASR reserves the longest accepted audio")
	require.Equal(t, body, req.Body, "the checked upload is forwarded unchanged")
	require.True(t, strings.HasPrefix(req.ContentType, "multipart/form-data; boundary="))
	contentType, body = deepSeekMultipart(t, nil, "a.ogg", deepSeekOggOpus(119.9))
	_, err = PrepareDeepSeekToolRequest(DeepSeekToolASR, contentType, body)
	require.NoError(t, err)

	req, err = PrepareDeepSeekToolRequest(DeepSeekToolTTS, "", []byte(`{"text":"你好"}`))
	require.NoError(t, err)
	require.Equal(t, 4, req.EstimateUnits)
	require.Equal(t, "你好", req.AuditText)

	req, err = PrepareDeepSeekToolRequest(DeepSeekToolFetch, "", []byte(`{"url":"https://example.com/x"}`))
	require.NoError(t, err)
	require.Equal(t, "https://example.com/x", req.AuditText)
	require.Equal(t, 1, req.EstimateUnits)
}

// Only a well-formed single image is reserved as one page; anything that may
// hold several pages is reserved at the supplier's maximum.
func TestDeepSeekOCRReservesThePageMaximumUnlessSingleImage(t *testing.T) {
	bmp := func(size int, dib uint32) []byte {
		b := make([]byte, size)
		copy(b, "BM")
		binary.LittleEndian.PutUint32(b[2:], uint32(size))
		binary.LittleEndian.PutUint32(b[10:], 54)
		binary.LittleEndian.PutUint32(b[14:], dib)
		return b
	}
	for _, tc := range []struct {
		name  string
		file  []byte
		pages int
	}{
		{"pdf", []byte("%PDF-1.4\n1 0 obj << /Type /Page >> endobj"), 1000},
		{"pdf_object_streams", append([]byte("%PDF-1.5 "), make([]byte, 300<<10)...), 1000},
		{"office", append([]byte("PK\x03\x04"), make([]byte, 10)...), 1000},
		{"tiff", []byte("II*\x00tiff"), 1000},
		{"gif", []byte("GIF89a..."), 1000},
		{"png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDRdata"), 1},
		{"png_without_ihdr", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dXXXXdata"), 1000},
		{"jpeg", []byte("\xFF\xD8\xFF\xE0\x00\x10JFIF"), 1},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 data"), 1},
		{"webp_extended", []byte("RIFF\x00\x00\x00\x00WEBPVP8Xdata"), 1000},
		{"bmp", bmp(64, 40), 1},
		{"bmp_magic_only", append([]byte("BM"), make([]byte, 62)...), 1000},
		{"bmp_unknown_header", bmp(64, 7), 1000},
		{"image_hiding_a_pdf", append([]byte("\xFF\xD8\xFF\xE0 %PDF-1.4 "), make([]byte, 20)...), 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contentType, body := deepSeekMultipart(t, map[string]string{"purpose": "text_recognition"}, "f", tc.file)
			req, err := PrepareDeepSeekToolRequest(DeepSeekToolOCR, contentType, body)
			require.NoError(t, err)
			require.Equal(t, tc.pages, req.EstimateUnits)
			require.Equal(t, 1000, req.MaxUnits)
		})
	}
}

// The reservation covers whatever page count the supplier can confirm, so a
// user whose balance covers a few pages cannot start a document that is
// charged for hundreds.
func TestDeepSeekOCRReservationIsAnUpperBound(t *testing.T) {
	file := append([]byte("%PDF-1.7 "), make([]byte, 300<<10)...) // say, 500 pages in object streams
	contentType, body := deepSeekMultipart(t, map[string]string{"purpose": "text_recognition"}, "big.pdf", file)
	t.Run("refused_when_the_maximum_is_not_covered", func(t *testing.T) {
		h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true)}, func(deepSeekToolsCall) (*http.Response, error) {
			t.Fatal("no supplier call may be made")
			return nil, nil
		})
		rec := httptest.NewRecorder()
		// 1,000 pages at 3 per 1,000 need 3; this user has 1.
		h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 1), DeepSeekToolOCR, contentType, body)
		require.Equal(t, http.StatusPaymentRequired, rec.Code)
	})
	t.Run("reserves_the_maximum_and_charges_the_confirmed_pages", func(t *testing.T) {
		var reserved float64
		var h *deepSeekToolsHarness
		h = newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true)}, func(deepSeekToolsCall) (*http.Response, error) {
			h.cache.mu.Lock()
			for _, amount := range h.cache.res {
				reserved += amount
			}
			h.cache.mu.Unlock()
			return deepSeekJSONResponse(200, `{"pages":[{"text":"x"}],"page_count":500}`), nil
		})
		rec := httptest.NewRecorder()
		h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolOCR, contentType, body)
		require.Equal(t, http.StatusOK, rec.Code)
		require.InDelta(t, 3.0, reserved, 1e-12, "1,000 pages at 3 per 1,000")
		require.InDelta(t, 1.5, h.journal.settled(t).Command.BalanceCost, 1e-12, "500 confirmed pages")
		h.requireReservationReleased(t)
	})
}

// The audio length a client declares is only checked before sending; the
// charge follows the supplier's answer, bounded by what the file size allows.
func TestDeepSeekASRChargesTheSuppliersDurationNotTheDeclaredOne(t *testing.T) {
	forged := deepSeekWAV(5)                                        // 160,000 bytes of 16 kHz audio
	binary.LittleEndian.PutUint32(forged[28:], uint32(len(forged))) // the header claims about one second
	seconds, err := deepSeekAudioSeconds(forged)
	require.NoError(t, err)
	require.Less(t, seconds, 1.1)
	limit := (len(forged) + 749) / 750
	for _, tc := range []struct {
		durationMS float64
		seconds    int
	}{{150000, 150}, {1e10, limit}} {
		t.Run(fmt.Sprint(tc.durationMS), func(t *testing.T) {
			h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true)}, func(deepSeekToolsCall) (*http.Response, error) {
				return deepSeekJSONResponse(200, fmt.Sprintf(`{"text":"long speech","duration_ms":%v}`, tc.durationMS)), nil
			})
			rec := httptest.NewRecorder()
			contentType, body := deepSeekMultipart(t, nil, "a.wav", forged)
			h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolASR, contentType, body)
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, tc.seconds, h.journal.settled(t).Usage.BillingSnapshot.DeepSeekTools.ASRSeconds)
		})
	}
}

// A client that reads slowly until the gateway's own deadline ends the stream
// still pays for the audio it got; only the supplier cutting it short is free.
func TestDeepSeekTTSGatewayDeadlineIsCharged(t *testing.T) {
	saved := deepSeekToolTimeouts[DeepSeekToolTTS]
	deepSeekToolTimeouts[DeepSeekToolTTS] = 100 * time.Millisecond
	t.Cleanup(func() { deepSeekToolTimeouts[DeepSeekToolTTS] = saved })
	h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true)}, func(call deepSeekToolsCall) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte("RIFFfirst"))
			<-call.ctx.Done() // the rest would arrive after the gateway's deadline
			_ = pw.CloseWithError(call.ctx.Err())
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"audio/wav"}}, Body: pr}, nil
	})
	rec := httptest.NewRecorder()
	h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolTTS, "", []byte(`{"text":"hello"}`))
	require.Equal(t, "RIFFfirst", rec.Body.String())
	settled := h.journal.settled(t)
	require.NotNil(t, settled)
	require.Equal(t, 5, settled.Usage.BillingSnapshot.DeepSeekTools.TTSCharacters)
	h.requireReservationReleased(t)
}

// One account setting enables all five tools; an upstream without this tool
// answers 404 or 405 and another account is tried.
func TestDeepSeekToolsFailOverWhenAnUpstreamLacksTheTool(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			first := true
			h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true), deepSeekToolsTestAccount(12, true)}, func(deepSeekToolsCall) (*http.Response, error) {
				if first {
					first = false
					return deepSeekJSONResponse(status, `{"error":{"message":"no such route"}}`), nil
				}
				return deepSeekJSONResponse(200, `{"pages":[{"text":"x"}],"page_count":1}`), nil
			})
			rec := httptest.NewRecorder()
			contentType, body := deepSeekMultipart(t, map[string]string{"purpose": "text_recognition"}, "a.png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR"))
			h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolOCR, contentType, body)
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, 2, h.callCount())
			require.Equal(t, 1, h.journal.settled(t).Usage.BillingSnapshot.DeepSeekTools.OCRPages)
		})
	}
}

// A 200 whose body is cut short or unreadable may follow completed, metered
// work: it is not charged, and clients are told not to retry it.
func TestDeepSeekToolsUnreadableSuccessIsNotRetried(t *testing.T) {
	pdfType, pdfBody := deepSeekMultipart(t, map[string]string{"purpose": "text_recognition"}, "a.pdf", []byte("%PDF-1.4"))
	wavType, wavBody := deepSeekMultipart(t, nil, "a.wav", deepSeekWAV(1))
	for name, tc := range map[string]struct {
		op, contentType string
		body            []byte
		answer          func() *http.Response
	}{
		"ocr_cut_short": {DeepSeekToolOCR, pdfType, pdfBody, func() *http.Response {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(io.MultiReader(strings.NewReader(`{"pages":[`), iotestErrReader{}))}
		}},
		"ocr_not_json":    {DeepSeekToolOCR, pdfType, pdfBody, func() *http.Response { return deepSeekJSONResponse(200, `{"pages":[`) }},
		"asr_not_json":    {DeepSeekToolASR, wavType, wavBody, func() *http.Response { return deepSeekJSONResponse(200, `<html>`) }},
		"search_not_json": {DeepSeekToolSearch, "application/json", []byte(`{"query":"x"}`), func() *http.Response { return deepSeekJSONResponse(200, `{"results":`) }},
	} {
		t.Run(name, func(t *testing.T) {
			h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true), deepSeekToolsTestAccount(12, true)}, func(deepSeekToolsCall) (*http.Response, error) { return tc.answer(), nil })
			rec := httptest.NewRecorder()
			h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), tc.op, tc.contentType, tc.body)
			require.Equal(t, 1, h.callCount())
			require.Equal(t, http.StatusBadGateway, rec.Code)
			require.Equal(t, "false", rec.Header().Get("x-should-retry"))
			require.Nil(t, h.journal.settled(t))
			h.requireReservationReleased(t)
		})
	}
}

// The audio's length is announced only with the audio itself.
func TestDeepSeekTTSFailingFirstReadSendsNoAudioLength(t *testing.T) {
	h := newDeepSeekToolsHarness(t, []Account{deepSeekToolsTestAccount(11, true)}, func(deepSeekToolsCall) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"audio/wav"}, "Content-Length": []string{"48000"}}, Body: io.NopCloser(iotestErrReader{})}, nil
	})
	rec := httptest.NewRecorder()
	h.serve(t, context.Background(), rec, deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10), DeepSeekToolTTS, "", []byte(`{"text":"hello"}`))
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Empty(t, rec.Header().Get("Content-Length"))
	require.True(t, gjson.Valid(rec.Body.String()))
	require.Equal(t, "false", rec.Header().Get("x-should-retry"))
	require.Nil(t, h.journal.settled(t))
}

func TestDeepSeekAudioDuration(t *testing.T) {
	seconds, err := deepSeekAudioSeconds(deepSeekWAV(2.5))
	require.NoError(t, err)
	require.InDelta(t, 2.5, seconds, 1e-9)
	seconds, err = deepSeekAudioSeconds(deepSeekOggOpus(3))
	require.NoError(t, err)
	require.InDelta(t, 3, seconds, 1e-9)
}

// Hosted tools use the model account when it supplies tools itself, or the
// pinned account; they never borrow another tool account of the group.
func TestDeepSeekHostedToolAccountChoice(t *testing.T) {
	model, tools := deepSeekToolsTestAccount(11, false), deepSeekToolsTestAccount(12, true)
	h := newDeepSeekToolsHarness(t, []Account{model, tools}, func(deepSeekToolsCall) (*http.Response, error) { return nil, errors.New("unused") })
	key := deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10)
	_, _, err := h.svc.deepSeekToolAccount(context.Background(), key, &model)
	require.Error(t, err, "a model account without tools leaves the tools unavailable")

	supplier, release, err := h.svc.deepSeekToolAccount(context.Background(), key, &tools)
	require.NoError(t, err)
	require.Equal(t, int64(12), supplier.ID)
	release()

	pinned := int64(12)
	key.Group.DeepSeekTools.AccountID = &pinned
	supplier, release, err = h.svc.deepSeekToolAccount(context.Background(), key, &model)
	require.NoError(t, err)
	require.Equal(t, int64(12), supplier.ID, "a pinned tool account still serves any model account")
	release()
}

// deepSeekProjectedAccountRepo lists accounts the way the scheduler snapshot
// does: a metadata projection whose Extra lacks deepseek_tools_enabled. Loading
// one account by ID returns the full row.
type deepSeekProjectedAccountRepo struct{ stubOpenAIAccountRepo }

func (r deepSeekProjectedAccountRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]Account, error) {
	accounts, err := r.stubOpenAIAccountRepo.ListSchedulableByGroupIDAndPlatform(ctx, groupID, platform)
	projected := make([]Account, len(accounts))
	for i, account := range accounts {
		account.Extra = map[string]any{"quota_limit": 0.0}
		projected[i] = account
	}
	return projected, err
}

func TestDeepSeekToolAccountsAreRecognizedBehindTheSchedulerProjection(t *testing.T) {
	accounts := []Account{deepSeekToolsTestAccount(11, false), deepSeekToolsTestAccount(12, true)}
	h := newDeepSeekToolsHarness(t, accounts, func(call deepSeekToolsCall) (*http.Response, error) {
		require.Equal(t, int64(12), call.account, "only the tool account may be used")
		return deepSeekJSONResponse(200, `{"results":[{"url":"https://example.com/a"}]}`), nil
	})
	h.svc.accountRepo = deepSeekProjectedAccountRepo{stubOpenAIAccountRepo{accounts: accounts}}
	key := deepSeekToolsTestKey(deepSeekToolsAllPrices(), 10)
	rec := httptest.NewRecorder()
	h.serve(t, context.Background(), rec, key, DeepSeekToolSearch, "", []byte(`{"query":"x"}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, h.callCount())

	// A hosted model account handed over as a projection is still recognized.
	projected := accounts[1]
	projected.Extra = map[string]any{}
	supplier, release, err := h.svc.deepSeekToolAccount(context.Background(), key, &projected)
	require.NoError(t, err)
	require.Equal(t, int64(12), supplier.ID)
	release()
}
