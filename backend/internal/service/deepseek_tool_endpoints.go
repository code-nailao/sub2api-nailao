package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// Standalone tool endpoints are thin relays: one supplier call per attempt,
// no gateway tool loop. A definitive rejection before anything was relayed
// moves to another tool account; a transport failure after the request was
// sent is never retried, because the supplier may already have done the work.
const (
	deepSeekToolMaxAttempts  = 4
	deepSeekOCRResponseLimit = 64 << 20
	deepSeekToolErrorLimit   = 64 << 10
)

var (
	errDeepSeekToolOutcomeUnknown = errors.New("DeepSeek tool transport failed; operation was not retried")
	errDeepSeekToolIncomplete     = errors.New("incomplete or oversized DeepSeek tool response")
)

func deepSeekToolTimeout(operation string) time.Duration {
	switch operation {
	case DeepSeekToolOCR:
		return 10 * time.Minute
	case DeepSeekToolASR:
		return 5 * time.Minute
	case DeepSeekToolTTS:
		return 15 * time.Minute
	}
	return 60 * time.Second
}

// DeepSeekToolGroupPrice returns the group's configured price for a tool in its
// own unit (per 1,000 calls or pages, per hour, per 10,000 characters); nil
// means the group does not offer the tool.
func DeepSeekToolGroupPrice(group *Group, operation string) *float64 {
	if group == nil {
		return nil
	}
	cfg := group.DeepSeekTools
	switch operation {
	case DeepSeekToolSearch:
		return group.SearchPricePer1k
	case DeepSeekToolFetch:
		return cfg.FetchPricePer1K
	case DeepSeekToolOCR:
		return cfg.OCRPricePer1KPages
	case DeepSeekToolASR:
		return cfg.ASRPricePerHour
	case DeepSeekToolTTS:
		return cfg.TTSPricePer10KChars
	}
	return nil
}

// setDeepSeekToolUnits records confirmed units of one standalone operation.
func setDeepSeekToolUnits(u *DeepSeekToolUsage, operation string, units int) {
	switch operation {
	case DeepSeekToolSearch:
		u.SearchCount = units
	case DeepSeekToolFetch:
		u.FetchCount = units
	case DeepSeekToolOCR:
		u.OCRPages = units
	case DeepSeekToolASR:
		u.ASRSeconds = units
	case DeepSeekToolTTS:
		u.TTSCharacters = units
	}
}

// deepSeekStandaloneSnapshot adds the operation's own price and supplier cost
// to the Search/Fetch snapshot. Other tools' prices are left out, so a
// Search/Fetch receipt is unchanged.
func deepSeekStandaloneSnapshot(key *APIKey, account *Account, operation string) (*DeepSeekToolUsage, error) {
	u, err := deepSeekToolSnapshot(key, account)
	if err != nil {
		return nil, err
	}
	price := DeepSeekToolGroupPrice(key.Group, operation)
	if price == nil {
		return nil, fmt.Errorf("DeepSeek %s is not offered for this group", operation)
	}
	switch operation {
	case DeepSeekToolOCR:
		u.OCRPricePer1KPages, u.SupplierOCRPricePer1KPages = *price, deepSeekSupplierCost(account, "deepseek_ocr_cost_per_1k_pages")
	case DeepSeekToolASR:
		u.ASRPricePerHour, u.SupplierASRPricePerHour = *price, deepSeekSupplierCost(account, "deepseek_asr_cost_per_hour")
	case DeepSeekToolTTS:
		u.TTSPricePer10KChars, u.SupplierTTSPricePer10KChars = *price, deepSeekSupplierCost(account, "deepseek_tts_cost_per_10k_chars")
	}
	u.Standalone = true
	return u, nil
}

// DeepSeekToolEstimate is the amount reserved before the call: the request's
// estimated units at the group's fixed price, without any rate multiplier.
func DeepSeekToolEstimate(group *Group, req *DeepSeekToolRequest) (float64, bool) {
	price := DeepSeekToolGroupPrice(group, req.Operation)
	if price == nil {
		return 0, false
	}
	u := &DeepSeekToolUsage{SearchPrice: *price / 1000, FetchPrice: *price / 1000, OCRPricePer1KPages: *price, ASRPricePerHour: *price, TTSPricePer10KChars: *price}
	setDeepSeekToolUnits(u, req.Operation, req.EstimateUnits)
	return u.Cost(), true
}

// deepSeekToolRejection is an HTTP error answer from the supplier, or a
// request that could not be built, before anything was relayed to the client.
type deepSeekToolRejection struct {
	status  int // 0: never sent
	message string
}

func (e *deepSeekToolRejection) Error() string {
	if e.status == 0 {
		return e.message
	}
	return fmt.Sprintf("DeepSeek tool supplier returned HTTP %d", e.status)
}

// failover reports a refusal that says the supplier did not do the work on this
// account (credentials, balance, rate limit, availability), so another tool
// account may be tried. Gateway timeouts (502/504) leave the outcome unknown.
func (e *deepSeekToolRejection) failover() bool {
	switch e.status {
	case 0, http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusServiceUnavailable:
		return true
	}
	return false
}

// inputRejected reports a refusal of the request itself, which another account
// would refuse as well.
func (e *deepSeekToolRejection) inputRejected() bool {
	switch e.status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// deepSeekToolReply is one supplier answer with HTTP 200.
type deepSeekToolReply struct {
	units       int    // confirmed billable units; 0 is not charged
	body        []byte // JSON to send after settlement (nil once audio streamed)
	contentType string
	committed   bool // the status line was written (OCR heartbeat or audio)
	streamed    bool // the audio itself was written
	clientGone  bool
}

func (r *deepSeekToolReply) commit(w http.ResponseWriter) {
	if r.committed {
		return
	}
	w.Header().Set("Content-Type", r.contentType)
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	r.committed = true
}

// write sends bytes to the client; a vanished client is remembered, not an error.
func (r *deepSeekToolReply) write(w http.ResponseWriter, p []byte) {
	if r.clientGone {
		return
	}
	r.commit(w)
	if _, err := w.Write(p); err != nil {
		r.clientGone = true
		return
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// WriteDeepSeekToolError writes the error shape shared by all tool endpoints.
func WriteDeepSeekToolError(w http.ResponseWriter, status int, typ, message, requestID string, noRetry bool) {
	body := map[string]string{"type": typ, "message": message}
	if requestID != "" {
		body["request_id"] = requestID
	}
	data, _ := json.Marshal(map[string]any{"error": body})
	if noRetry {
		// OpenAI SDKs retry 408, 409, 429 and 5xx on their own.
		w.Header().Set("x-should-retry", "false")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// ServeDeepSeekTool runs one validated standalone tool request for key and
// writes the response. meta carries the request details for the usage row.
//
// The estimate is reserved first and released exactly once when this returns,
// after settlement. Only units confirmed by a successful supplier answer are
// charged, at the group's fixed price. The returned error is a billing
// admission failure, returned before anything is written, for the caller to map.
func (s *OpenAIGatewayService) ServeDeepSeekTool(ctx context.Context, w http.ResponseWriter, key *APIKey, req *DeepSeekToolRequest, meta OpenAIRecordUsageInput) error {
	if err := s.checkDeepSeekToolsEnabled(key); err != nil {
		WriteDeepSeekToolError(w, http.StatusServiceUnavailable, "deepseek_tool_error", err.Error(), "", false)
		return nil
	}
	estimate, offered := DeepSeekToolEstimate(key.Group, req)
	if !offered {
		WriteDeepSeekToolError(w, http.StatusForbidden, "deepseek_tool_error", "DeepSeek "+req.Operation+" is not offered for this group", "", false)
		return nil
	}
	if key.User == nil || key.User.Balance < estimate || (key.Quota > 0 && key.Quota-key.QuotaUsed < estimate) {
		WriteDeepSeekToolError(w, http.StatusPaymentRequired, "deepseek_tool_error", "insufficient balance or API key quota for tool operation", "", false)
		return nil
	}
	if s.billingCacheService != nil {
		reservation, err := s.billingCacheService.ReserveFixedCharge(ctx, key.User, key, key.Group, nil, estimate)
		if err != nil {
			return err
		}
		// Released after settlement, or when no supplier answer is billed.
		defer reservation.HandlerDone()
		ctx = WithInflightReservation(ctx, reservation)
	}
	excluded := map[int64]struct{}{}
	var last *deepSeekToolRejection
	for attempt := 0; attempt < deepSeekToolMaxAttempts; attempt++ {
		account, release, err := s.selectDeepSeekToolAccount(ctx, key, excluded)
		if err != nil {
			if last == nil {
				WriteDeepSeekToolError(w, http.StatusServiceUnavailable, "deepseek_tool_error", err.Error(), "", false)
				return nil
			}
			break
		}
		rejection, done := s.attemptDeepSeekTool(ctx, w, key, req, meta, account)
		release()
		if done {
			return nil
		}
		// Nothing was relayed or charged; another account may serve the request.
		excluded[account.ID] = struct{}{}
		last = rejection
		logger.L().Warn("deepseek tools: supplier refused, trying another tool account",
			zap.String("operation", req.Operation), zap.Int64("account_id", account.ID), zap.Int("upstream_status", rejection.status))
	}
	status := http.StatusBadGateway
	if last.status == http.StatusTooManyRequests {
		status = http.StatusTooManyRequests
	}
	WriteDeepSeekToolError(w, status, "deepseek_tool_error", last.Error(), "", false)
	return nil
}

// attemptDeepSeekTool makes one supplier call on account and finishes the
// request, unless the supplier refused in a way another account may not
// (done=false): then nothing was written and nothing is charged.
func (s *OpenAIGatewayService) attemptDeepSeekTool(ctx context.Context, w http.ResponseWriter, key *APIKey, req *DeepSeekToolRequest, meta OpenAIRecordUsageInput, account *Account) (*deepSeekToolRejection, bool) {
	started := time.Now()
	usage, err := deepSeekStandaloneSnapshot(key, account, req.Operation)
	if err != nil {
		WriteDeepSeekToolError(w, http.StatusServiceUnavailable, "deepseek_tool_error", err.Error(), "", false)
		return nil, true
	}
	result := &OpenAIForwardResult{RequestID: "deepseek-tools:" + uuid.NewString(), Model: "deepseek-tools-" + req.Operation, DeepSeekTools: usage}
	input := meta
	input.Result, input.APIKey, input.User, input.Account = result, key, key.User, account
	input.UpstreamEndpoint = "/v1/tools/" + req.Operation
	input.RequestPayloadHash = HashUsageRequestPayload(req.Body)
	input.QuotaPlatform = PlatformDeepseek
	// A pending journal entry that is never completed settles as nothing after
	// its deadline, which outlives the longest supplier call.
	window := deepSeekToolTimeout(req.Operation)
	if window < deepSeekRequestTimeout {
		window = deepSeekRequestTimeout
	}
	deadline := time.Now().Add(window + time.Minute)
	if err = s.beginDeepSeekToolUsageUntil(ctx, &input, deadline); err != nil {
		WriteDeepSeekToolError(w, http.StatusServiceUnavailable, "deepseek_tool_error", "tool billing journal unavailable", "", false)
		return nil, true
	}
	w.Header().Set("X-Sources-Request-Id", result.RequestID)
	reply, err := s.relayDeepSeekTool(ctx, w, account, req)
	if err != nil {
		var rejection *deepSeekToolRejection
		if errors.As(err, &rejection) {
			if rejection.failover() {
				return rejection, false
			}
			if rejection.inputRejected() {
				msg := "the tool provider rejected the request"
				if rejection.message != "" {
					msg += ": " + truncateString(rejection.message, 256)
				}
				WriteDeepSeekToolError(w, http.StatusBadRequest, "invalid_request_error", msg, "", false)
				return nil, true
			}
		}
		s.failDeepSeekTool(w, reply, err, result.RequestID)
		return nil, true
	}
	setDeepSeekToolUnits(usage, req.Operation, reply.units)
	result.Duration = time.Since(started)
	// Settle before returning a buffered result. A disconnect cannot cancel
	// accounting for an already completed supplier operation.
	billingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err = s.RecordUsage(billingCtx, &input); err != nil {
		logger.L().Error("deepseek tools: usage settlement pending", zap.String("request_id", result.RequestID), zap.String("operation", req.Operation), zap.Error(err))
		if reply.streamed {
			return nil, true // the audio is out; the journal keeps the confirmed usage
		}
		message := "Tool completed but billing could not be confirmed; do not automatically retry."
		if reply.committed {
			reply.write(w, deepSeekToolErrorBody("tool_billing_pending", message, result.RequestID))
		} else {
			WriteDeepSeekToolError(w, http.StatusUnprocessableEntity, "tool_billing_pending", message, result.RequestID, true)
		}
		return nil, true
	}
	if reply.streamed {
		return nil, true
	}
	if reply.committed {
		reply.write(w, reply.body)
		return nil, true
	}
	w.Header().Set("Content-Type", reply.contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(reply.body)
	return nil, true
}

func deepSeekToolErrorBody(typ, message, requestID string) []byte {
	data, _ := json.Marshal(map[string]any{"error": map[string]string{"type": typ, "message": message, "request_id": requestID}})
	return data
}

// failDeepSeekTool reports a failed or unknown supplier outcome. Nothing is
// charged; the client is told not to replay the paid request automatically.
func (s *OpenAIGatewayService) failDeepSeekTool(w http.ResponseWriter, reply *deepSeekToolReply, err error, requestID string) {
	message := err.Error()
	var rejection *deepSeekToolRejection
	if errors.As(err, &rejection) && rejection.message != "" {
		message += ": " + truncateString(rejection.message, 256)
	}
	switch {
	case reply != nil && reply.streamed:
		// Audio was already sent and cannot be withdrawn; the response ends here.
		logger.L().Warn("deepseek tools: audio stream interrupted", zap.String("request_id", requestID), zap.Error(err))
	case reply != nil && reply.committed:
		reply.write(w, deepSeekToolErrorBody("deepseek_tool_error", message, requestID))
	default:
		// A lost connection or a gateway timeout leaves the outcome unknown.
		unknown := errors.Is(err, errDeepSeekToolOutcomeUnknown) || (rejection != nil && (rejection.status == http.StatusBadGateway || rejection.status == http.StatusGatewayTimeout))
		WriteDeepSeekToolError(w, http.StatusBadGateway, "deepseek_tool_error", message, requestID, unknown)
	}
}

// relayDeepSeekTool makes the supplier call. OCR and ASR keep running after a
// client disconnect so confirmed work is billed; TTS stops with the client.
func (s *OpenAIGatewayService) relayDeepSeekTool(ctx context.Context, w http.ResponseWriter, account *Account, req *DeepSeekToolRequest) (*deepSeekToolReply, error) {
	upstreamCtx := ctx
	if req.Operation == DeepSeekToolOCR || req.Operation == DeepSeekToolASR {
		upstreamCtx = context.WithoutCancel(ctx)
	}
	upstreamCtx, cancel := context.WithTimeout(upstreamCtx, deepSeekToolTimeout(req.Operation))
	defer cancel()
	accept := "application/json"
	if req.Operation == DeepSeekToolTTS {
		accept = "audio/wav, audio/pcm, application/octet-stream, application/json"
	}
	httpReq, proxy, err := s.deepSeekToolEndpoint(upstreamCtx, account, req.Operation, req.ContentType, accept, req.Body)
	if err != nil {
		return nil, &deepSeekToolRejection{message: err.Error()}
	}
	resp, err := s.httpUpstream.Do(httpReq, proxy, account.ID, account.Concurrency)
	if err != nil {
		return nil, errDeepSeekToolOutcomeUnknown
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, deepSeekToolErrorLimit))
		return nil, &deepSeekToolRejection{status: resp.StatusCode, message: sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(data))}
	}
	switch req.Operation {
	case DeepSeekToolOCR:
		return relayDeepSeekOCR(w, resp.Body)
	case DeepSeekToolTTS:
		return relayDeepSeekTTS(ctx, cancel, w, resp, req.EstimateUnits)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, deepSeekToolResponseLimit+1))
	if err != nil || len(data) > deepSeekToolResponseLimit {
		return nil, errDeepSeekToolIncomplete
	}
	if !gjson.ValidBytes(data) {
		return nil, errors.New("invalid DeepSeek tool response")
	}
	reply := &deepSeekToolReply{body: data, contentType: "application/json"}
	switch req.Operation {
	case DeepSeekToolASR:
		reply.units = deepSeekASRBillableSeconds(data)
	default:
		if deepSeekToolSucceeded(req.Operation, resp.StatusCode, data) {
			reply.units = 1
		}
	}
	return reply, nil
}

// relayDeepSeekOCR forwards the whitespace the supplier sends while it works,
// so the client connection stays alive, and buffers the JSON result for
// settlement before it is sent.
func relayDeepSeekOCR(w http.ResponseWriter, body io.Reader) (*deepSeekToolReply, error) {
	reply := &deepSeekToolReply{contentType: "application/json"}
	buf := make([]byte, 32<<10)
	var data []byte
	for {
		n, err := body.Read(buf)
		chunk := buf[:n]
		if len(data) == 0 && n > 0 {
			i := 0
			for i < n && (chunk[i] == ' ' || chunk[i] == '\n' || chunk[i] == '\r' || chunk[i] == '\t') {
				i++
			}
			if i > 0 {
				reply.write(w, chunk[:i])
			}
			chunk = chunk[i:]
		}
		if len(data)+len(chunk) > deepSeekOCRResponseLimit {
			return reply, errDeepSeekToolIncomplete
		}
		data = append(data, chunk...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return reply, errDeepSeekToolIncomplete
		}
	}
	if !gjson.ValidBytes(data) {
		return reply, errors.New("invalid DeepSeek tool response")
	}
	reply.body = data
	reply.units = deepSeekOCRBillablePages(data)
	return reply, nil
}

// deepSeekOCRBillablePages is page_count of a result with at least one page;
// an error envelope or an empty result is not charged.
func deepSeekOCRBillablePages(data []byte) int {
	if gjson.GetBytes(data, "error").Exists() {
		return 0
	}
	count, pages := gjson.GetBytes(data, "page_count"), gjson.GetBytes(data, "pages")
	if count.Type != gjson.Number || count.Int() < 1 || float64(count.Int()) != count.Float() || !pages.IsArray() || len(pages.Array()) == 0 {
		return 0
	}
	// The supplier accepts at most this many pages; never charge more.
	return int(min(count.Int(), deepSeekOCRMaxPages))
}

// deepSeekASRBillableSeconds is duration_ms rounded up to whole seconds, for a
// result with recognized text; never more than the accepted audio length.
func deepSeekASRBillableSeconds(data []byte) int {
	if gjson.GetBytes(data, "error").Exists() {
		return 0
	}
	text, duration := gjson.GetBytes(data, "text"), gjson.GetBytes(data, "duration_ms")
	if text.Type != gjson.String || strings.TrimSpace(text.String()) == "" || duration.Type != gjson.Number || duration.Float() <= 0 || math.IsInf(duration.Float(), 0) {
		return 0
	}
	return int(min(math.Ceil(duration.Float()/1000), deepSeekASRMaxSeconds))
}

// relayDeepSeekTTS streams the audio through. The request's billable
// characters are charged once the supplier has started sending audio and the
// stream ended or the client left; a JSON answer, an empty answer or an audio
// stream the supplier cuts short is not charged.
func relayDeepSeekTTS(ctx context.Context, cancel context.CancelFunc, w http.ResponseWriter, resp *http.Response, characters int) (*deepSeekToolReply, error) {
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") || strings.HasPrefix(mediaType, "text/") {
		data, err := io.ReadAll(io.LimitReader(resp.Body, deepSeekToolErrorLimit+1))
		if err != nil || len(data) > deepSeekToolErrorLimit {
			return nil, errDeepSeekToolIncomplete
		}
		if !gjson.ValidBytes(data) {
			return nil, errors.New("invalid DeepSeek tool response")
		}
		return &deepSeekToolReply{body: data, contentType: "application/json"}, nil
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	reply := &deepSeekToolReply{contentType: contentType}
	if length := resp.Header.Get("Content-Length"); length != "" {
		w.Header().Set("Content-Length", length)
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			reply.streamed = true
			reply.write(w, buf[:n])
			if reply.clientGone {
				cancel()
				break
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				reply.clientGone = true
				break
			}
			return reply, errors.New("DeepSeek TTS audio was interrupted by the provider")
		}
	}
	if !reply.streamed {
		w.Header().Del("Content-Length")
		return nil, errors.New("DeepSeek TTS returned no audio")
	}
	reply.units = characters
	return reply, nil
}
