package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/tidwall/gjson"
)

const (
	// UpstreamRejectionProfileGrokContentV1 is the canonical content-rejection
	// profile id. It is also the rule version written to new evidence.
	UpstreamRejectionProfileGrokContentV1 = "grok_content_rejection_v1"
	upstreamRejectionTextTail             = 256
	upstreamRejectionMaxOutputs           = 64
)

// upstreamRejectionLegacyProfile holds an older profile id that a deployment
// already stored in ledger rows, usage extras and account settings before the
// canonical id existed. It comes from private configuration only, so the public
// code carries no deployment-specific identifier. Empty means a fresh install.
var upstreamRejectionLegacyProfile atomic.Pointer[string]

// SetUpstreamRejectionLegacyProfile publishes the configured legacy profile id.
// Account and evidence helpers are plain functions without access to the
// configuration, so the service that owns it publishes a process snapshot at
// construction (as SetCodexIdentityEnforcementEnabled does). The value has been
// validated by config.Validate; "" clears it.
func SetUpstreamRejectionLegacyProfile(id string) {
	upstreamRejectionLegacyProfile.Store(&id)
}

func upstreamRejectionLegacyProfileID() string {
	if p := upstreamRejectionLegacyProfile.Load(); p != nil {
		return *p
	}
	return ""
}

// UpstreamRejectionProfileID is the profile id to write and to compare against.
// A configured legacy id keeps new rows identical to the rows already stored.
func UpstreamRejectionProfileID() string {
	if legacy := upstreamRejectionLegacyProfileID(); legacy != "" {
		return legacy
	}
	return UpstreamRejectionProfileGrokContentV1
}

// UpstreamRejectionRuleVersion is the rule version written to evidence. The
// stored rule version historically equals the profile id, so it follows it.
func UpstreamRejectionRuleVersion() string {
	return UpstreamRejectionProfileID()
}

// IsUpstreamRejectionProfile reports whether a stored or configured profile id
// names this profile: the canonical id, or the configured legacy id.
func IsUpstreamRejectionProfile(id string) bool {
	return id != "" && (id == UpstreamRejectionProfileGrokContentV1 || IsUpstreamRejectionLegacyProfile(id))
}

// IsUpstreamRejectionLegacyProfile reports whether id is the configured legacy
// profile id. Callers that show a profile replace it with the canonical id.
func IsUpstreamRejectionLegacyProfile(id string) bool {
	legacy := upstreamRejectionLegacyProfileID()
	return legacy != "" && id == legacy
}

// UpstreamRejectionEvidence contains only the rule and a digest of its evidence,
// never a prompt, response body, credential or customer-controlled price.
type UpstreamRejectionEvidence struct {
	Profile      string `json:"profile"`
	RuleID       string `json:"rule_id"`
	RuleVersion  string `json:"rule_version"`
	HTTPStatus   int    `json:"http_status"`
	ResponseHash string `json:"response_hash"`
}

func (a *Account) GetUpstreamRejectionProfile() string {
	if a == nil || a.Platform != PlatformGrok || a.Type != AccountTypeAPIKey {
		return ""
	}
	if p, ok := a.Extra["upstream_rejection_profile"].(string); ok && IsUpstreamRejectionProfile(p) {
		return UpstreamRejectionProfileID()
	}
	return ""
}

func (a *Account) GetUpstreamRejectionCost() float64 {
	if a.GetUpstreamRejectionProfile() == "" {
		return 0
	}
	raw, ok := a.Extra["upstream_rejection_cost"]
	if !ok {
		return 0.1
	}
	var n float64
	switch v := raw.(type) {
	case float64:
		n = v
	case float32:
		n = float64(v)
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case json.Number:
		var err error
		n, err = v.Float64()
		if err != nil {
			return math.NaN()
		}
	case string:
		var err error
		n, err = strconv.ParseFloat(v, 64)
		if err != nil {
			return math.NaN()
		}
	default:
		return math.NaN()
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		// Invalid configuration must fail financial admission, rather than be
		// confused with an explicitly configured zero supplier cost.
		return math.NaN()
	}
	return n
}

func (a *Account) GetUpstreamRejectionCurrency() string {
	if a.GetUpstreamRejectionProfile() == "" {
		return ""
	}
	if s, ok := a.Extra["upstream_rejection_currency"].(string); ok {
		return strings.ToUpper(strings.TrimSpace(s))
	}
	return "USD"
}

var upstreamRejectionQuotes = strings.NewReplacer("’", "'", "‘", "'")

func normalizeUpstreamRejectionText(s string) string {
	return strings.ToLower(upstreamRejectionQuotes.Replace(s))
}

var upstreamRejectionPhrases = []string{
	"i'm sorry, i cannot help with", "i'm sorry, i can't help with",
	"sorry, i cannot help with", "sorry, i can't help with",
	"i cannot help with", "i can't help with",
}
var upstreamRejectionSuffixes = []string{"that", "this", "you", "your", "the request", "the prompt", "it"}
var upstreamRejection403Phrases = []string{
	"content violates usage guidelines", "safety_check_type_", "content moderation", "content policy",
	"image is sensitive", "text is sensitive", "prohibited content", "forbidden content",
	"content policy violation", "content policy rejection", "content policy rejected",
	"content moderation rejection", "content moderation rejected", "content moderation blocked",
	"request blocked by content moderation", "request rejected by content moderation",
	"request blocked by policy", "request rejected by policy", "request violates policy",
	"prompt violates content policy", "prompt violates policy", "input violates content policy", "input violates policy",
}

// matchUpstreamRefusal uses the supplier's literal phrases, not a wildcard
// interpretation of the redacted c***t/ca*** announcement. Only intervening
// whitespace is stripped; punctuation and word boundaries are not invented.
func matchUpstreamRefusal(text string) (string, string) {
	for i, phrase := range upstreamRejectionPhrases {
		start := 0
		for start < len(text) {
			at := strings.Index(text[start:], phrase)
			if at < 0 {
				break
			}
			at += start
			suffix := strings.TrimLeftFunc(text[at+len(phrase):], unicode.IsSpace)
			for _, allowed := range upstreamRejectionSuffixes {
				if strings.HasPrefix(suffix, allowed) {
					return "refusal_phrase_" + strconv.Itoa(i+1), phrase + " " + allowed
				}
			}
			start = at + len(phrase)
		}
	}
	return "", ""
}

func upstreamRejectionEvidence(status int, rule, evidence string) *UpstreamRejectionEvidence {
	sum := sha256.Sum256([]byte(evidence))
	return &UpstreamRejectionEvidence{Profile: UpstreamRejectionProfileID(), RuleID: rule, RuleVersion: UpstreamRejectionRuleVersion(), HTTPStatus: status, ResponseHash: hex.EncodeToString(sum[:])}
}

// DetectUpstreamRejectionHTTP applies the original HTTP status. JSON extraction
// is allowlisted: request echoes, tool arguments, HTML and arbitrary metadata
// never become evidence. 2xx responses use only assistant output fields.
func DetectUpstreamRejectionHTTP(account *Account, status int, body []byte) *UpstreamRejectionEvidence {
	if account.GetUpstreamRejectionProfile() == "" {
		return nil
	}
	if status >= 200 && status < 300 {
		o := NewUpstreamRejectionObserver(account, status)
		o.ObserveJSON(body)
		return o.Evidence()
	}
	if status != 400 && status != 403 {
		return nil
	}
	for _, candidate := range upstreamRejectionErrorTexts(body) {
		text := normalizeUpstreamRejectionText(candidate)
		if status == 400 {
			for i, phrase := range []string{"generated image rejected by content moderation", "generated video rejected by content moderation"} {
				if strings.Contains(text, phrase) {
					return upstreamRejectionEvidence(status, "media_rejected_"+strconv.Itoa(i+1), candidate)
				}
			}
			continue
		}
		for i, phrase := range upstreamRejection403Phrases {
			if strings.Contains(text, phrase) {
				return upstreamRejectionEvidence(status, "policy_"+strconv.Itoa(i+1), candidate)
			}
		}
		if rule, _ := matchUpstreamRefusal(text); rule != "" {
			return upstreamRejectionEvidence(status, rule, candidate)
		}
	}
	return nil
}

func upstreamRejectionErrorTexts(body []byte) []string {
	if !json.Valid(body) {
		s := strings.TrimSpace(string(body))
		lower := strings.ToLower(s)
		// A malformed JSON body or an HTML/WAF page is not a trusted error message.
		if s == "" || strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") || strings.Contains(lower, "<html") || strings.Contains(lower, "<!doctype") || strings.ContainsAny(s, "<>") {
			return nil
		}
		return []string{s}
	}
	root := gjson.ParseBytes(body)
	paths := []string{"error", "error.message", "error.error", "error.code", "error.reason", "error.type", "message", "detail", "code", "reason", "response.error", "response.error.message", "response.error.code", "response.error.reason"}
	texts := make([]string, 0, len(paths))
	for _, path := range paths {
		r := root.Get(path)
		if r.Type == gjson.String {
			texts = append(texts, r.String())
		}
	}
	return texts
}

// UpstreamRejectionObserver incrementally observes decoded assistant output.
// Per-output tails are bounded and never joined across choices/content parts.
// One positive result is sticky, so repeated full/delta terminal events cannot
// produce another charge. The caller owns settlement and event idempotency.
type UpstreamRejectionObserver struct {
	enabled  bool
	status   int
	tails    map[string]string
	roles    map[string]string
	evidence *UpstreamRejectionEvidence
}

func NewUpstreamRejectionObserver(account *Account, status int) *UpstreamRejectionObserver {
	return &UpstreamRejectionObserver{enabled: account.GetUpstreamRejectionProfile() != "", status: status}
}

func (o *UpstreamRejectionObserver) Evidence() *UpstreamRejectionEvidence {
	if o == nil || o.evidence == nil {
		return nil
	}
	copy := *o.evidence
	return &copy
}

func (o *UpstreamRejectionObserver) ObserveText(outputID, text string, final bool) {
	if o == nil || !o.enabled || o.evidence != nil || o.status < 200 || o.status >= 300 || text == "" {
		return
	}
	if o.tails == nil {
		o.tails = make(map[string]string)
	}
	_, existingOutput := o.tails[outputID]
	canStore := existingOutput || len(o.tails) < upstreamRejectionMaxOutputs
	text = normalizeUpstreamRejectionText(text)
	if !final {
		text = o.tails[outputID] + text
	}
	if rule, _ := matchUpstreamRefusal(text); rule != "" {
		o.evidence = upstreamRejectionEvidence(o.status, rule, text)
		return
	}
	if final {
		delete(o.tails, outputID)
		return
	}
	if !canStore {
		return
	}
	// Collapse only the unbounded separator following a complete literal phrase.
	// This preserves a pending match across arbitrarily long whitespace without
	// rewriting the phrase itself or allowing punctuation where it is forbidden.
	for _, phrase := range upstreamRejectionPhrases {
		if at := strings.LastIndex(text, phrase); at >= 0 {
			suffix := strings.TrimLeftFunc(text[at+len(phrase):], unicode.IsSpace)
			for _, allowed := range upstreamRejectionSuffixes {
				if strings.HasPrefix(allowed, suffix) {
					text = phrase + " " + suffix
					break
				}
			}
		}
	}
	if len(text) > upstreamRejectionTextTail {
		text = text[len(text)-upstreamRejectionTextTail:]
	}
	o.tails[outputID] = text
}

func upstreamRejectionString(value gjson.Result) string {
	if value.Type != gjson.String {
		return ""
	}
	return value.String()
}

func (o *UpstreamRejectionObserver) observeContent(id string, content gjson.Result, final bool) {
	if content.Type == gjson.String {
		o.ObserveText(id, content.String(), final)
		return
	}
	if !content.IsArray() {
		return
	}
	for i, p := range content.Array() {
		t := p.Get("type").String()
		switch t {
		case "text", "output_text":
			o.ObserveText(id+":"+strconv.Itoa(i), upstreamRejectionString(p.Get("text")), final)
		case "refusal":
			o.ObserveText(id+":"+strconv.Itoa(i), upstreamRejectionString(p.Get("refusal")), final)
		}
	}
}

func (o *UpstreamRejectionObserver) ObserveJSON(body []byte) {
	if o == nil || !o.enabled || o.evidence != nil || !json.Valid(body) {
		return
	}
	if o.status < 200 || o.status >= 300 {
		return
	}
	root := gjson.ParseBytes(body)
	o.observeRoot(root)
}

func (o *UpstreamRejectionObserver) observeRoot(root gjson.Result) {
	if o == nil || o.evidence != nil {
		return
	}
	for i, c := range root.Get("choices").Array() {
		msg := c.Get("message")
		role := msg.Get("role").String()
		if role != "" && role != "assistant" {
			continue
		}
		index := strconv.Itoa(i)
		if c.Get("index").Exists() {
			index = c.Get("index").String()
		}
		o.observeContent("chat:"+index, msg.Get("content"), true)
		if r := msg.Get("refusal"); r.Type == gjson.String {
			o.ObserveText("chat-refusal:"+index, r.String(), true)
		}
	}
	// output_text is the Responses API convenience field, not user input.
	if r := root.Get("output_text"); r.Type == gjson.String {
		o.ObserveText("response:output_text", r.String(), true)
	}
	for i, item := range root.Get("output").Array() {
		o.observeResponseItem(item, "response:"+strconv.Itoa(i), true)
	}
}

func (o *UpstreamRejectionObserver) observeResponseItem(item gjson.Result, id string, final bool) {
	role := item.Get("role").String()
	if item.Get("type").String() != "message" || role != "assistant" {
		return
	}
	o.observeContent(id, item.Get("content"), final)
}

// responseOutputIdentity never coalesces unidentified or distinct items. Some
// compatible providers omit output_index but preserve item_id. Without either,
// only the complete current fragment can be examined; joining is unsafe.
func responseRejectionOutputIdentity(root gjson.Result) (string, bool) {
	if index := root.Get("output_index"); index.Exists() && index.Type == gjson.Number {
		return "response:index:" + index.String(), true
	}
	for _, path := range []string{"item_id", "item.id"} {
		if id := root.Get(path); id.Type == gjson.String && id.String() != "" && len(id.String()) <= 256 {
			return "response:item:" + id.String(), true
		}
	}
	return "response:unidentified", false
}

func responseRejectionContentIdentity(root gjson.Result) (string, bool) {
	id, stable := responseRejectionOutputIdentity(root)
	part := "0"
	if index := root.Get("content_index"); index.Exists() && index.Type == gjson.Number {
		part = index.String()
	}
	return id + ":" + part, stable
}

func (o *UpstreamRejectionObserver) ObserveSSEData(data []byte) {
	if o == nil || !o.enabled || o.evidence != nil || o.status < 200 || o.status >= 300 || !json.Valid(data) {
		return
	}
	root := gjson.ParseBytes(data)
	typ := root.Get("type").String()
	switch typ {
	case "response.output_text.delta", "response.refusal.delta":
		id, stable := responseRejectionContentIdentity(root)
		o.ObserveText(id, upstreamRejectionString(root.Get("delta")), !stable)
	case "response.output_text.done", "response.refusal.done":
		id, _ := responseRejectionContentIdentity(root)
		field := "text"
		if typ == "response.refusal.done" {
			field = "refusal"
		}
		o.ObserveText(id, upstreamRejectionString(root.Get(field)), true)
	case "response.output_item.added", "response.output_item.done":
		item := root.Get("item")
		id, stable := responseRejectionOutputIdentity(root)
		o.observeResponseItem(item, id, !stable || typ == "response.output_item.done")
	case "response.content_part.added", "response.content_part.done":
		part := root.Get("part")
		id, _ := responseRejectionContentIdentity(root)
		if part.Get("type").String() == "output_text" {
			o.ObserveText(id, upstreamRejectionString(part.Get("text")), true)
		}
		if part.Get("type").String() == "refusal" {
			o.ObserveText(id, upstreamRejectionString(part.Get("refusal")), true)
		}
	case "response.completed", "response.done", "response.incomplete", "response.failed":
		o.observeRoot(root.Get("response"))
	case "error", "response.error":
		// HTTP 200 SSE error is NOT an HTTP 403. An error's text is not assistant output.
		return
	default:
		// Chat Completions events have no type. Refuse arbitrary typed events.
		if typ != "" {
			return
		}
		for i, c := range root.Get("choices").Array() {
			index := strconv.Itoa(i)
			if c.Get("index").Exists() {
				index = c.Get("index").String()
			}
			id := "chat:" + index
			delta := c.Get("delta")
			if o.roles == nil {
				o.roles = make(map[string]string)
			}
			_, known := o.roles[id]
			if !known && len(o.roles) >= upstreamRejectionMaxOutputs {
				// Do not forget a newly seen tool/user role and later treat its
				// role-less continuation as an assistant. Whole messages with an
				// explicit role can still be examined by observeRoot below.
				continue
			}
			if !known {
				o.roles[id] = ""
			}
			if role := delta.Get("role").String(); role != "" {
				o.roles[id] = role
			}
			if role := o.roles[id]; role != "" && role != "assistant" {
				continue
			}
			o.observeContent(id, delta.Get("content"), false)
			if r := delta.Get("refusal"); r.Type == gjson.String {
				o.ObserveText("chat-refusal:"+index, r.String(), false)
			}
		}
		// Some compatible servers send a final full message instead of a delta.
		o.observeRoot(root)
	}
}
