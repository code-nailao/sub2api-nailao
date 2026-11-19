package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"strings"

	"sources.local/mediaengine"
)

// This request grammar is specific to the retained media engine JSON protocol. It does
// not alter upstream's ordinary JSON/multipart image parser or response handling.
type sourcesMediaEngineRequest struct {
	Model, Prompt, Quality, Size, Hash string
	fields                             map[string]json.RawMessage
	refs                               []string
	raw                                []byte
}

var sourcesMediaEngineModels = map[string]bool{"gpt-image-2": true, "gpt-image-medium": true, "gemini-2.5-flash-image": true, "gemini-3-pro-image": true, "gemini-3-pro-image-preview": true, "gemini-3.1-flash-image-preview": true}

// SourcesImageRequestError contains only public request-validation guidance.
// Provider and transport failures must not be exposed as validation messages.
type SourcesImageRequestError struct{ message string }

func (e *SourcesImageRequestError) Error() string { return e.message }

func parseSourcesMediaEngineRequest(endpoint, contentType string, body []byte) (_ *sourcesMediaEngineRequest, requestErr error) {
	defer func() {
		if requestErr != nil {
			requestErr = &SourcesImageRequestError{message: requestErr.Error()}
		}
	}()
	bad := func(msg string) (*sourcesMediaEngineRequest, error) { return nil, errors.New(msg) }
	media, _, err := mime.ParseMediaType(contentType)
	if contentType != "" && (err != nil || media != "application/json") {
		return bad("asynchronous image requests require JSON; use JSON image references for edits")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return bad("image request must be a JSON object")
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return bad("invalid image request")
		}
		key, ok := token.(string)
		if !ok || len(key) > 128 || len(fields) >= 128 {
			return bad("invalid image request field")
		}
		if _, exists := fields[key]; exists {
			return bad("duplicate image request field")
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil {
			return bad("invalid image request value")
		}
		fields[key] = raw
	}
	if _, err = d.Token(); err != nil {
		return bad("invalid image request")
	}
	var tail any
	if d.Decode(&tail) != io.EOF {
		return bad("unexpected image request tail")
	}
	str := func(key string) (string, error) {
		raw, ok := fields[key]
		if !ok {
			return "", nil
		}
		var v string
		if json.Unmarshal(raw, &v) != nil {
			return "", errors.New(key + " must be a string")
		}
		return strings.TrimSpace(v), nil
	}
	model, err := str("model")
	if err != nil {
		return nil, err
	}
	if _, ok := fields["model"]; !ok {
		model = "gpt-image-2"
	}
	model = strings.ToLower(model)
	if !sourcesMediaEngineModels[model] {
		return bad("selected model does not support asynchronous image generation")
	}
	prompt, err := str("prompt")
	if err != nil || prompt == "" {
		return bad("prompt is required")
	}
	if raw, ok := fields["n"]; ok {
		var n int
		if json.Unmarshal(raw, &n) != nil || n != 1 {
			return bad("asynchronous image requests support n=1")
		}
	}
	if raw, ok := fields["stream"]; ok {
		var stream bool
		if json.Unmarshal(raw, &stream) != nil || stream {
			return bad("asynchronous image requests do not support streaming")
		}
	}
	if raw, ok := fields["response_format"]; ok {
		var format string
		if json.Unmarshal(raw, &format) != nil || !strings.EqualFold(strings.TrimSpace(format), "url") {
			return bad("asynchronous image requests require response_format=url")
		}
	}
	if _, ok := fields["mask"]; ok {
		return bad("asynchronous image edits do not support masks")
	}
	r := &sourcesMediaEngineRequest{Model: model, Prompt: prompt, Size: "2K", fields: fields, raw: body}
	r.Quality, err = str("quality")
	if err != nil {
		return nil, err
	}
	r.Quality = strings.ToLower(r.Quality)
	if _, ok := fields["quality"]; ok && (model == "gpt-image-2" || model == "gpt-image-medium") && r.Quality != "low" && r.Quality != "medium" && r.Quality != "high" {
		return bad("quality must be low, medium, or high")
	}
	selected := ""
	for _, key := range []string{"output_resolution", "image_size", "size"} {
		v, e := str(key)
		if e != nil {
			return nil, e
		}
		if v == "" {
			continue
		}
		tier, ok := sourcesMediaEngineSize(key, v)
		if !ok {
			return bad("unsupported image resolution")
		}
		if selected != "" && selected != tier {
			return bad("image resolution fields disagree")
		}
		selected = tier
	}
	if selected != "" {
		r.Size = selected
	}
	var add func(json.RawMessage) error
	add = func(raw json.RawMessage) error {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			if value == "" {
				return errors.New("empty image reference")
			}
			r.refs = append(r.refs, value)
			if len(r.refs) > 4 {
				return errors.New("at most four image references")
			}
			return nil
		}
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) == nil && list != nil {
			for _, v := range list {
				if err := add(v); err != nil {
					return err
				}
			}
			return nil
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) == nil && object != nil {
			for _, key := range []string{"url", "image_url", "source"} {
				if v, ok := object[key]; ok {
					return add(v)
				}
			}
		}
		return errors.New("unsupported image reference")
	}
	for _, key := range []string{"image", "images", "reference_images"} {
		if raw, ok := fields[key]; ok {
			if err := add(raw); err != nil {
				return nil, err
			}
		}
	}
	if strings.HasSuffix(endpoint, "/edits/async") && len(r.refs) == 0 {
		return bad("image reference required for edits")
	}
	canonical, _ := json.Marshal(fields)
	r.Hash = HashUsageRequestPayload(append([]byte(endpoint+"\x00"), canonical...))
	if _, err := r.body(r.Model, nil); err != nil {
		return nil, err
	}
	return r, nil
}
func sourcesMediaEngineSize(field, value string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(value))
	if field == "output_resolution" {
		switch v {
		case "1k":
			return "1K", true
		case "2k":
			return "2K", true
		case "4k":
			return "4K", true
		default:
			return "", false
		}
	}
	switch v {
	case "1024x1024", "1376x768", "768x1376":
		return "1K", true
	case "2048x2048", "2752x1536", "1536x2752", "2560x1440", "1440x2560":
		return "2K", true
	case "4096x4096", "5504x3072", "3072x5504", "3840x2160", "2160x3840":
		return "4K", true
	}
	return ClassifyImageBillingTier(value)
}
func (r *sourcesMediaEngineRequest) body(model string, refs []string) ([]byte, error) {
	out := map[string]json.RawMessage{}
	for _, k := range []string{"prompt", "size", "image_size", "output_resolution", "aspect_ratio", "output_format"} {
		if v, ok := r.fields[k]; ok {
			var value string
			if json.Unmarshal(v, &value) != nil {
				return nil, errors.New(k + " must be a string")
			}
			out[k] = v
		}
	}
	if r.Model == "gpt-image-2" || r.Model == "gpt-image-medium" {
		for _, k := range []string{"quality", "background"} {
			if v, ok := r.fields[k]; ok {
				var value string
				if json.Unmarshal(v, &value) != nil {
					return nil, errors.New(k + " must be a string")
				}
				out[k] = v
			}
		}
	}
	out["model"], _ = json.Marshal(model)
	if len(refs) > 0 {
		out["images"], _ = json.Marshal(refs)
	}
	return json.Marshal(out)
}
func sourcesMediaEnginePricingModel(ns mediaengine.Namespace, model, quality string) string {
	root := sourcesMediaEnginePriceRoot(ns.Name())
	switch model {
	case "gpt-image-2":
		if quality == "high" {
			return root + "gpt-image-2/high"
		}
		return root + "gpt-image-2/standard"
	case "gpt-image-medium":
		return root + "gpt-image-medium"
	}
	return model
}

// SourcesMediaEngineAuditInput validates the independent grammar and adapts every
// supported reference alias to the native image moderation input, before I/O.
func SourcesMediaEngineAuditInput(endpoint, contentType string, body []byte) (string, []byte, error) {
	r, err := parseSourcesMediaEngineRequest(endpoint, contentType, body)
	if err != nil {
		return "", nil, err
	}
	payload := map[string]any{"prompt": r.Prompt}
	images := make([]map[string]string, 0, len(r.refs))
	for _, ref := range r.refs {
		images = append(images, map[string]string{"image_url": ref})
	}
	payload["images"] = images
	encoded, err := json.Marshal(payload)
	return r.Model, encoded, err
}
