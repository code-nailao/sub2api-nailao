package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

const deepSeekIncrementalKey = "sources.deepseek.incremental"

// Only hosted output is intercepted. Requests, upstream selection and confirmed
// per-turn billing continue through the existing gateway. A single public
// lifecycle spans all internal turns; neither private calls nor their terminals
// are forwarded. The heartbeat and output writer share the same lock.
type deepSeekIncremental struct {
	toolEvents map[string]map[string]any
	c          *gin.Context
	protocol   string
	owned      map[string]string
	heartbeat  *deepSeekHeartbeat
	result     *OpenAIForwardResult
	started    time.Time
	created    bool
	sequence   int
	outputs    []apicompat.ResponsesOutput
	chat       *apicompat.ResponsesEventToChatState
	messages   *apicompat.ResponsesEventToAnthropicState
	writeErr   error
	bytes      int
}

func newDeepSeekIncremental(c *gin.Context, protocol string, owned map[string]string, h *deepSeekHeartbeat, result *OpenAIForwardResult, started time.Time) *deepSeekIncremental {
	chat := apicompat.NewResponsesEventToChatState()
	chat.IncludeUsage = false
	return &deepSeekIncremental{c: c, protocol: protocol, owned: owned, heartbeat: h, result: result, started: started, toolEvents: map[string]map[string]any{}, chat: chat, messages: apicompat.NewResponsesEventToAnthropicState()}
}

func (s *deepSeekIncremental) emit(e apicompat.ResponsesStreamEvent) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	if err := s.c.Request.Context().Err(); err != nil {
		s.result.ClientDisconnect = true
		return err
	}
	e.SequenceNumber = s.sequence
	s.sequence++
	var frames []string
	switch s.protocol {
	case "chat":
		for _, chunk := range apicompat.ResponsesEventToChatChunks(&e, s.chat) {
			frame, err := apicompat.ChatChunkToSSE(chunk)
			if err != nil {
				return err
			}
			frames = append(frames, frame)
		}
	case "messages":
		var blocks []json.RawMessage
		if e.Item != nil && e.Type == "response.output_item.done" {
			if tool := s.toolEvents[e.Item.ID]; tool != nil {
				raw, err := finishDeepSeekHostedResponse("messages", []byte(`{"content":[]}`), s.result, []map[string]any{tool})
				if err != nil {
					return err
				}
				var parsed struct {
					Content []json.RawMessage `json:"content"`
				}
				_ = json.Unmarshal(raw, &parsed)
				blocks = parsed.Content
			}
		}
		for _, event := range apicompat.ResponsesEventToAnthropicEvents(&e, s.messages) {
			if event.Type == "content_block_start" && len(blocks) > 0 {
				raw, _ := json.Marshal(event)
				var value map[string]any
				_ = json.Unmarshal(raw, &value)
				value["content_block"] = blocks[0]
				blocks = blocks[1:]
				raw, _ = json.Marshal(value)
				frames = append(frames, "event: content_block_start\ndata: "+string(raw)+"\n\n")
				continue
			}
			frame, err := apicompat.ResponsesAnthropicEventToSSE(event)
			if err != nil {
				return err
			}
			frames = append(frames, frame)
		}
	default:
		frame, err := apicompat.ResponsesEventToSSE(e)
		if err != nil {
			return err
		}
		frames = append(frames, frame)
	}
	if e.Response != nil && (e.Type == "response.completed" || e.Type == "response.incomplete") && s.protocol != "chat" {
		for i, frame := range frames {
			split := strings.Index(frame, "data: ")
			if split < 0 {
				continue
			}
			var value map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(frame[split+6:])), &value) != nil {
				continue
			}
			target := value
			if s.protocol == "responses" {
				if response, ok := value["response"].(map[string]any); ok {
					target = response
				}
			}
			if usage, ok := target["usage"].(map[string]any); ok {
				usage["server_tool_use"] = map[string]int{"tool_search": s.result.DeepSeekTools.SearchCount, "tool_fetch": s.result.DeepSeekTools.FetchCount}
				raw, _ := json.Marshal(value)
				frames[i] = frame[:split] + "data: " + string(raw) + "\n\n"
			}
		}
	}
	if len(frames) == 0 {
		return nil
	}
	s.heartbeat.mu.Lock()
	defer s.heartbeat.mu.Unlock()
	s.c.Writer.Header().Set("Content-Type", "text/event-stream")
	s.c.Writer.Header().Set("Cache-Control", "no-cache")
	s.c.Writer.Header().Set("X-Accel-Buffering", "no")
	for _, frame := range frames {
		if _, err := s.c.Writer.WriteString(frame); err != nil {
			s.writeErr = err
			s.result.ClientDisconnect = true
			return err
		}
	}
	s.c.Writer.Flush()
	visible := e.Delta != "" && (e.Type == "response.output_text.delta" || e.Type == "response.refusal.delta" || e.Type == "response.reasoning_text.delta" || e.Type == "response.reasoning_summary_text.delta" || e.Type == "response.function_call_arguments.delta")
	if visible && s.result.FirstTokenMs == nil {
		ms := int(time.Since(s.started).Milliseconds())
		s.result.FirstTokenMs = &ms
	}
	return nil
}

func (s *deepSeekIncremental) create(r *apicompat.ResponsesResponse) error {
	if s.created {
		return nil
	}
	s.created = true
	start := *r
	start.ID = s.result.RequestID
	start.Status = "in_progress"
	start.Output = []apicompat.ResponsesOutput{}
	start.Usage = nil
	return s.emit(apicompat.ResponsesStreamEvent{Type: "response.created", Response: &start})
}

// Only function calls are held until their complete identity and arguments are
// known. Text and reasoning never wait for an internal turn's terminal.
type deepSeekIncrementalTurn struct {
	parent    *deepSeekIncremental
	protocol  string
	pending   []byte
	bytes     int
	chat      *apicompat.ChatCompletionsToResponsesStreamState
	messages  *apicompat.AnthropicEventToResponsesState
	terminal  *apicompat.ResponsesResponse
	indices   map[int]int
	functions map[int]bool
	err       error
}

func (s *deepSeekIncremental) turn(protocol string) *deepSeekIncrementalTurn {
	return &deepSeekIncrementalTurn{parent: s, protocol: protocol, chat: apicompat.NewChatCompletionsToResponsesStreamState(s.result.Model), messages: apicompat.NewAnthropicEventToResponsesState(), indices: map[int]int{}, functions: map[int]bool{}}
}
func (w *deepSeekIncrementalTurn) write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	w.bytes += len(p)
	w.parent.bytes += len(p)
	if w.bytes > deepSeekToolResponseLimit || w.parent.bytes > deepSeekToolResponseLimit {
		w.err = errors.New("DeepSeek streamed response exceeds buffer limit")
		return 0, w.err
	}
	w.pending = append(w.pending, p...)
	for {
		i, n := bytes.Index(w.pending, []byte("\n\n")), 2
		if cr := bytes.Index(w.pending, []byte("\r\n\r\n")); cr >= 0 && (i < 0 || cr < i) {
			i, n = cr, 4
		}
		if i < 0 {
			break
		}
		frame := append([]byte(nil), w.pending[:i]...)
		w.pending = w.pending[i+n:]
		if err := w.frame(frame); err != nil {
			w.err = err
			return 0, err
		}
	}
	return len(p), nil
}
func (w *deepSeekIncrementalTurn) frame(frame []byte) error {
	var data []string
	for _, line := range strings.Split(string(frame), "\n") {
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if len(data) == 0 {
		return nil
	}
	raw := []byte(strings.Join(data, "\n"))
	if bytes.Equal(raw, []byte("[DONE]")) {
		if w.protocol == "chat" {
			if w.chat.FinishReason == "" {
				return errors.New("DeepSeek chat stream has no finish reason")
			}
			return w.events(apicompat.FinalizeChatCompletionsResponsesStream(w.chat))
		}
		return nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("invalid DeepSeek SSE: %w", err)
	}
	if _, ok := envelope["error"]; ok {
		return errors.New("DeepSeek stream returned an error")
	}
	switch w.protocol {
	case "chat":
		var chunk apicompat.ChatCompletionsChunk
		if err := json.Unmarshal(raw, &chunk); err != nil {
			return err
		}
		return w.events(apicompat.ChatCompletionsChunkToResponsesEvents(&chunk, w.chat))
	case "messages":
		var event apicompat.AnthropicStreamEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return err
		}
		if event.Type == "error" {
			return errors.New("DeepSeek messages stream failed")
		}
		return w.events(apicompat.AnthropicEventToResponsesEvents(&event, w.messages))
	default:
		var event apicompat.ResponsesStreamEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return err
		}
		return w.events([]apicompat.ResponsesStreamEvent{event})
	}
}
func (w *deepSeekIncrementalTurn) index(i int) int {
	if n, ok := w.indices[i]; ok {
		return n
	}
	n := len(w.parent.outputs)
	w.indices[i] = n
	w.parent.outputs = append(w.parent.outputs, apicompat.ResponsesOutput{})
	return n
}
func (w *deepSeekIncrementalTurn) publicItem(item apicompat.ResponsesOutput, i int) apicompat.ResponsesOutput {
	item.ID = fmt.Sprintf("ds_item_%d", i)
	return item
}
func (w *deepSeekIncrementalTurn) events(events []apicompat.ResponsesStreamEvent) error {
	for _, e := range events {
		switch e.Type {
		case "error":
			return errors.New("DeepSeek stream failed")
		case "response.created", "response.in_progress":
			if e.Response != nil {
				if err := w.parent.create(e.Response); err != nil {
					return err
				}
			}
			continue
		case "response.completed", "response.done", "response.incomplete", "response.failed":
			if e.Response == nil {
				return errors.New("DeepSeek stream terminal has no response")
			}
			if w.terminal != nil {
				return errors.New("DeepSeek stream repeated its terminal")
			}
			w.terminal = e.Response
			if e.Type == "response.failed" || e.Response.Status == "failed" {
				return errors.New("DeepSeek model response failed")
			}
			continue
		}
		if e.Item != nil && e.Item.Type == "function_call" {
			w.functions[e.OutputIndex] = true
			continue
		}
		if w.functions[e.OutputIndex] || strings.HasPrefix(e.Type, "response.function_call_arguments.") {
			continue
		}
		if !strings.HasPrefix(e.Type, "response.output_") && !strings.HasPrefix(e.Type, "response.content_part.") && !strings.HasPrefix(e.Type, "response.reasoning_") && !strings.HasPrefix(e.Type, "response.refusal.") {
			continue
		}
		i := w.index(e.OutputIndex)
		e.OutputIndex = i
		if e.ItemID != "" {
			e.ItemID = fmt.Sprintf("ds_item_%d", i)
		}
		if e.Item != nil {
			item := w.publicItem(*e.Item, i)
			e.Item = &item
			if e.Type == "response.output_item.done" {
				w.parent.outputs[i] = item
			}
		}
		if err := w.parent.emit(e); err != nil {
			return err
		}
	}
	return nil
}

// Reconstruct only the bounded internal continuation snapshot. Never replay
// text already sent to the client. Public client functions are emitted once,
// after ownership is certain; private calls remain only in this snapshot.
func (w *deepSeekIncrementalTurn) finish() ([]byte, error) {
	if w.err != nil {
		return nil, w.err
	}
	if len(bytes.TrimSpace(w.pending)) > 0 {
		return nil, errors.New("DeepSeek stream ended in a partial SSE frame")
	}
	if w.terminal == nil {
		return nil, errors.New("DeepSeek stream ended without a terminal response")
	}
	r := w.terminal
	if r.Status != "completed" && r.Status != "incomplete" {
		return nil, errors.New("DeepSeek stream has invalid terminal status")
	}
	for original, item := range r.Output {
		if item.Type == "function_call" {
			if _, owned := w.parent.owned[item.Name]; owned {
				continue
			}
			if item.Name == "" || item.CallID == "" || !json.Valid([]byte(item.Arguments)) {
				return nil, errors.New("DeepSeek client function is incomplete")
			}
			i := w.index(original)
			item = w.publicItem(item, i)
			w.parent.outputs[i] = item
			initial := item
			initial.Arguments = ""
			for _, e := range []apicompat.ResponsesStreamEvent{
				{Type: "response.output_item.added", OutputIndex: i, Item: &initial},
				{Type: "response.function_call_arguments.delta", OutputIndex: i, ItemID: item.ID, CallID: item.CallID, Delta: item.Arguments},
				{Type: "response.function_call_arguments.done", OutputIndex: i, ItemID: item.ID, CallID: item.CallID, Arguments: item.Arguments},
				{Type: "response.output_item.done", OutputIndex: i, Item: &item},
			} {
				if err := w.parent.emit(e); err != nil {
					return nil, err
				}
			}
		} else {
			i := w.index(original)
			w.parent.outputs[i] = w.publicItem(item, i)
		}
	}
	switch w.protocol {
	case "chat":
		return json.Marshal(apicompat.ResponsesToChatCompletions(r, w.parent.result.Model))
	case "messages":
		return json.Marshal(apicompat.ResponsesToAnthropic(r, w.parent.result.Model))
	default:
		return json.Marshal(r)
	}
}

func (s *deepSeekIncremental) complete(capture *deepSeekCapture, result *OpenAIForwardResult, events []map[string]any) error {
	if capture.incremental == nil || capture.incremental.terminal == nil {
		return errors.New("DeepSeek stream terminal missing")
	}
	terminal := *capture.incremental.terminal
	terminal.ID = result.RequestID
	terminal.Output = s.outputs
	// Use the existing usage aggregator so tools and every model turn retain
	// their billing units; public metadata does not drive ledger settlement.
	raw, err := json.Marshal(terminal)
	if err != nil {
		return err
	}
	raw, err = finishDeepSeekHostedResponse("responses", raw, result, nil)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(raw, &terminal); err != nil {
		return err
	}
	if err = s.emit(apicompat.ResponsesStreamEvent{Type: "response." + terminal.Status, Response: &terminal}); err != nil {
		return err
	}
	if s.protocol == "chat" {
		s.heartbeat.mu.Lock()
		defer s.heartbeat.mu.Unlock()
		// Preserve hosted tool counters/metadata, which the generic converter does
		// not carry, alongside the final aggregate usage chunk.
		usageWire, _ := finishDeepSeekHostedResponse("chat", []byte(`{"choices":[]}`), result, events)
		var usage map[string]any
		_ = json.Unmarshal(usageWire, &usage)
		usage["id"] = s.chat.ID
		usage["object"] = "chat.completion.chunk"
		usage["created"] = s.chat.Created
		usage["model"] = s.chat.Model
		payload, _ := json.Marshal(usage)
		if _, err = fmt.Fprintf(s.c.Writer, "data: %s\n\ndata: [DONE]\n\n", payload); err != nil {
			s.result.ClientDisconnect = true
			return err
		}
		s.c.Writer.Flush()
	}
	return nil
}

// Publish confirmed tool results between rounds, using the existing hosted
// protocol rendering for Messages so Fetch and failure payloads retain meaning.
func (s *deepSeekIncremental) toolCompleted(event map[string]any) error {
	raw, err := json.Marshal(deepSeekPublicEvent(event))
	if err != nil {
		return err
	}
	var item apicompat.ResponsesOutput
	if err = json.Unmarshal(raw, &item); err != nil {
		return err
	}
	if s.toolEvents == nil {
		s.toolEvents = map[string]map[string]any{}
	}
	s.toolEvents[item.ID] = event
	i := len(s.outputs)
	s.outputs = append(s.outputs, item)
	if err = s.emit(apicompat.ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: i, Item: &item}); err != nil {
		return err
	}
	return s.emit(apicompat.ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: i, Item: &item})
}
