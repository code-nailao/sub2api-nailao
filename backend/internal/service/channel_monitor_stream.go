package service

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/tidwall/gjson"
)

// monitorStreamDecoder is local to one probe. Configuration remains the final
// request body's stream boolean; templates and stored monitor snapshots need no
// new fields. Merge mode's existing stream denylist deliberately stays unchanged.
type monitorStreamDecoder struct {
	responses    bool
	responseID   string
	text         strings.Builder
	chatFinished bool
}

func newMonitorStreamDecoder(provider, apiMode string, body []byte) (*monitorStreamDecoder, error) {
	value := gjson.GetBytes(body, "stream")
	if !value.Exists() || value.Type == gjson.False {
		return nil, nil
	}
	if value.Type != gjson.True {
		return nil, fmt.Errorf("monitor request: stream must be a boolean")
	}
	if !isOpenAICompatibleChatProvider(provider) {
		return nil, fmt.Errorf("streaming probes currently support OpenAI-compatible Chat Completions and Responses only")
	}
	return &monitorStreamDecoder{responses: provider == MonitorProviderOpenAI && apiMode == MonitorAPIModeResponses}, nil
}

// read dispatches complete SSE events, including multiline data and CRLF. The
// budget covers all wire bytes (also comments), not just accumulated text. EOF,
// partial frames and a 2xx header alone cannot establish a successful probe.
func (d *monitorStreamDecoder) read(body io.Reader) (string, error) {
	wire := monitorSSEReader{reader: bufio.NewReader(io.LimitReader(body, monitorResponseMaxBytes+1))}
	var data strings.Builder
	var event string
	for {
		firstLine := wire.readBytes == 0
		line, err := wire.readLine()
		if err != nil {
			if err == io.EOF {
				return "", fmt.Errorf("streaming probe: stream ended before completion")
			}
			return "", fmt.Errorf("read streaming body: %w", err)
		}
		if firstLine {
			line = strings.TrimPrefix(line, "\xef\xbb\xbf")
		}
		if line == "" {
			if data.Len() > 0 {
				done, err := d.consume(event, strings.TrimSuffix(data.String(), "\n"))
				if err != nil {
					return "", err
				}
				if done {
					return d.text.String(), nil
				}
			}
			data.Reset()
			event = ""
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			_, _ = data.WriteString(value)
			_ = data.WriteByte('\n')
		case "event":
			event = value
		}
	}
}

// SSE allows LF, CRLF and bare CR. Defer skipping CR's optional LF until the
// next read so a complete event never waits for another network byte.
type monitorSSEReader struct {
	reader    *bufio.Reader
	readBytes int
	skipLF    bool
}

func (r *monitorSSEReader) readLine() (string, error) {
	var line strings.Builder
	for {
		b, err := r.reader.ReadByte()
		if err != nil {
			return "", err
		}
		r.readBytes++
		if r.readBytes > monitorResponseMaxBytes {
			return "", fmt.Errorf("streaming probe: response size limit exceeded")
		}
		if r.skipLF && b == '\n' {
			r.skipLF = false
			continue
		}
		r.skipLF = b == '\r'
		if b == '\r' || b == '\n' {
			return line.String(), nil
		}
		_ = line.WriteByte(b)
	}
}

func (d *monitorStreamDecoder) consume(event, data string) (bool, error) {
	if strings.TrimSpace(data) == "[DONE]" {
		if d.responses || !d.chatFinished {
			return false, fmt.Errorf("streaming probe: stream ended before completion")
		}
		return true, nil
	}
	if !json.Valid([]byte(data)) {
		return false, fmt.Errorf("streaming probe: invalid JSON in SSE event")
	}
	value := gjson.Parse(data)
	if !value.IsObject() {
		return false, fmt.Errorf("streaming probe: expected JSON object in SSE event")
	}
	kind := value.Get("type").String()
	if event != "" && event != "message" && kind != "" && event != kind {
		return false, fmt.Errorf("streaming probe: conflicting SSE event type")
	}
	if event == "error" || kind == "error" || value.Get("error").Type != gjson.Null {
		return false, monitorStreamError("upstream error", value)
	}
	if d.responses {
		return d.consumeResponses(kind, value)
	}
	return d.consumeChat(value)
}

func (d *monitorStreamDecoder) consumeResponses(kind string, value gjson.Result) (bool, error) {
	if err := d.checkResponseID(value.Get("response.id").String()); err != nil {
		return false, err
	}
	switch kind {
	case "response.failed", "response.incomplete":
		return false, monitorStreamError(kind, value)
	case "response.output_text.delta":
		delta := value.Get("delta")
		if delta.Type != gjson.String {
			return false, fmt.Errorf("streaming probe: invalid text delta")
		}
		_, _ = d.text.WriteString(delta.String())
	case "response.completed":
		response := value.Get("response")
		if !response.IsObject() || response.Get("status").String() != "completed" || response.Get("error").Type != gjson.Null {
			return false, monitorStreamError("invalid completion status", value)
		}
		// Prefer the final snapshot over deltas to avoid duplicating text. Some
		// compatible upstreams omit output in the terminal event; retain deltas.
		if text := extractOpenAIResponsesText([]byte(response.Raw)); strings.TrimSpace(text) != "" {
			d.text.Reset()
			_, _ = d.text.WriteString(text)
		}
		return true, nil
	}
	return false, nil
}

func (d *monitorStreamDecoder) consumeChat(value gjson.Result) (bool, error) {
	if err := d.checkResponseID(value.Get("id").String()); err != nil {
		return false, err
	}
	choices := value.Get("choices")
	if !choices.IsArray() {
		return false, fmt.Errorf("streaming probe: missing Chat Completions choices")
	}
	for _, choice := range choices.Array() {
		if choice.Get("index").Int() != 0 {
			continue
		}
		content := choice.Get("delta.content")
		if content.Type != gjson.Null && content.Type != gjson.String {
			return false, fmt.Errorf("streaming probe: invalid chat text delta")
		}
		if content.String() != "" {
			if d.chatFinished {
				return false, fmt.Errorf("streaming probe: text received after finish")
			}
			_, _ = d.text.WriteString(content.String())
		}
		if reason := choice.Get("finish_reason").String(); reason != "" {
			if reason != "stop" {
				return false, fmt.Errorf("streaming probe: incomplete chat response (finish_reason=%s)", reason)
			}
			d.chatFinished = true
		}
	}
	return false, nil
}

func (d *monitorStreamDecoder) checkResponseID(id string) error {
	if id == "" {
		return nil
	}
	if d.responseID != "" && d.responseID != id {
		return fmt.Errorf("streaming probe: response ID changed within stream")
	}
	d.responseID = id
	return nil
}

// runCheckForModel applies the existing sanitizer and message limit to this
// error. Never include the raw SSE frame or arbitrary response fields.
func monitorStreamError(kind string, value gjson.Result) error {
	for _, path := range []string{"response.incomplete_details.reason", "response.error.message", "error.message", "message", "response.error.code", "error.code"} {
		if detail := value.Get(path); detail.Type == gjson.String && detail.String() != "" {
			return fmt.Errorf("streaming probe: %s: %s", kind, detail.String())
		}
	}
	return fmt.Errorf("streaming probe: %s", kind)
}
