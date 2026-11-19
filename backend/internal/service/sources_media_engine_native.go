package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"sources.local/mediaengine"
)

// Native image calls run inside the existing durable job and native money
// boundary. Legacy remote task IDs still use their original polling protocol.
type sourcesNativeImageProvider struct{ owner *SourcesMediaEngine }

func (p sourcesNativeImageProvider) Submit(ctx context.Context, c mediaengine.Credential, body json.RawMessage, idem string) (string, error) {
	return (mediaengine.Client{Transport: p.owner}).Submit(ctx, c, body, idem)
}
func (p sourcesNativeImageProvider) Poll(ctx context.Context, c mediaengine.Credential, ref string) (mediaengine.Poll, error) {
	return (mediaengine.Client{Transport: p.owner}).Poll(ctx, c, ref)
}

var sourcesNativeModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

func sourcesGeminiAspect(size, explicit string) (string, error) {
	ratios := []struct {
		name  string
		value float64
	}{{"1:1", 1}, {"16:9", 16.0 / 9}, {"9:16", 9.0 / 16}, {"4:3", 4.0 / 3}, {"3:4", 3.0 / 4}, {"3:2", 1.5}, {"2:3", 2.0 / 3}, {"4:5", .8}, {"5:4", 1.25}, {"21:9", 21.0 / 9}}
	if explicit != "" {
		for _, r := range ratios {
			if explicit == r.name {
				return explicit, nil
			}
		}
		return "", errors.New("unsupported image aspect ratio")
	}
	w, h, ok := parseImageBillingDimensions(size)
	if !ok {
		return "1:1", nil
	}
	value := float64(w) / float64(h)
	for _, r := range ratios {
		if math.Abs(value/r.value-1) < .025 {
			return r.name, nil
		}
	}
	return "", errors.New("unsupported image aspect ratio")
}

func sourcesNativeImagePath(platform, model string, edit bool) string {
	if platform == PlatformGemini {
		return "/v1beta/models/" + model + ":generateContent"
	}
	if edit {
		return "/v1/images/edits"
	}
	return "/v1/images/generations"
}

// The resolver reads only references already staged by Main before admission.
// A resolver failure is deferred before any generation request is sent.
func sourcesNativeImageRequest(platform, tier string, raw json.RawMessage, readImage func(string) ([]byte, string, error)) (string, []byte, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return "", nil, mediaengine.ErrRejected
	}
	text := func(key string) string { var v string; _ = json.Unmarshal(fields[key], &v); return v }
	var refs []string
	if v := fields["images"]; len(v) > 0 && json.Unmarshal(v, &refs) != nil {
		return "", nil, mediaengine.ErrRejected
	}
	if len(refs) > 4 || text("model") == "" || text("prompt") == "" {
		return "", nil, mediaengine.ErrRejected
	}
	path := sourcesNativeImagePath(platform, text("model"), len(refs) > 0)
	if platform == PlatformOpenAI {
		fields["n"] = json.RawMessage(`1`)
		fields["response_format"] = json.RawMessage(`"b64_json"`)
		// Freeze the same tier used in the existing billing ticket, including the
		// historical default when the caller omitted size.
		fields["output_resolution"], _ = json.Marshal(tier)
		body, err := json.Marshal(fields)
		return path, body, err
	}
	if platform != PlatformGemini || !sourcesNativeModel.MatchString(text("model")) {
		return "", nil, mediaengine.ErrRejected
	}
	size := text("size")
	if size == "" {
		size = text("image_size")
	}
	aspect, err := sourcesGeminiAspect(size, text("aspect_ratio"))
	if err != nil {
		return "", nil, mediaengine.ErrRejected
	}
	parts := []any{map[string]string{"text": text("prompt")}}
	total := 0
	for _, ref := range refs {
		data, mime, err := readImage(ref)
		if err != nil {
			return "", nil, mediaengine.ErrDeferred
		}
		total += len(data)
		if total > 20<<20 {
			return "", nil, mediaengine.ErrRejected
		}
		parts = append(parts, map[string]any{"inlineData": map[string]string{"mimeType": mime, "data": base64.StdEncoding.EncodeToString(data)}})
	}
	body, err := json.Marshal(map[string]any{"contents": []any{map[string]any{"role": "user", "parts": parts}}, "generationConfig": map[string]any{"responseModalities": []string{"TEXT", "IMAGE"}, "imageConfig": map[string]string{"aspectRatio": aspect, "imageSize": tier}}})
	return path, body, err
}

func sourcesNativeImageResult(platform string, raw []byte) (string, error) {
	var value struct {
		Data []struct {
			B64 string `json:"b64_json"`
			URL string `json:"url"`
		} `json:"data"`
		Candidates []struct {
			Content struct {
				Parts []struct {
					Inline struct {
						Data string `json:"data"`
						MIME string `json:"mimeType"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return "", errors.New("invalid native image response")
	}
	refs := []string{}
	if platform == PlatformOpenAI {
		if len(value.Data) != 1 {
			return "", errors.New("native image response must contain one image")
		}
		for _, v := range value.Data {
			if v.B64 != "" {
				refs = append(refs, "data:image/png;base64,"+v.B64)
			} else if v.URL != "" {
				refs = append(refs, v.URL)
			}
		}
	} else {
		for _, v := range value.Candidates {
			for _, part := range v.Content.Parts {
				if part.Inline.Data != "" {
					refs = append(refs, "data:"+part.Inline.MIME+";base64,"+part.Inline.Data)
				}
			}
		}
	}
	if len(refs) != 1 {
		return "", errors.New("native image response must contain one image")
	}
	if !strings.HasPrefix(refs[0], "data:") {
		u, err := url.Parse(refs[0])
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
			return "", errors.New("invalid native image result URL")
		}
	}
	return refs[0], nil
}

func (p sourcesNativeImageProvider) Generate(ctx context.Context, t *mediaengine.Task) ([]string, error) {
	ticket, err := sourcesImageTicket(t)
	if err != nil {
		return nil, mediaengine.ErrRejected
	}
	if err = mediaengine.ValidateCredential(t.Credential); err != nil {
		return nil, mediaengine.ErrRejected
	}
	tier := ImageBillingSize2K
	if ticket.Log.ImageSize != nil {
		tier = *ticket.Log.ImageSize
	}
	if tier != ImageBillingSize1K && tier != ImageBillingSize2K && tier != ImageBillingSize4K {
		return nil, mediaengine.ErrRejected
	}
	path, body, err := sourcesNativeImageRequest(ticket.Platform, tier, t.Body, func(ref string) ([]byte, string, error) { return p.owner.imageBytes(ctx, ref, 20<<20) })
	if err != nil {
		return nil, err
	}
	target, _ := url.Parse(t.Credential.BaseURL)
	target.Path = path
	target.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, mediaengine.ErrRejected
	}
	// Never let net/http replay a paid POST after a lost connection.
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+t.Credential.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Idempotency-Key", t.ID)
	response, err := p.owner.Do(req, t.Credential)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		switch response.StatusCode {
		case 400, 401, 403, 404, 413, 422:
			return nil, mediaengine.ErrRejected
		}
		return nil, errors.New("native image submission outcome unknown")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (48<<20)+1))
	if err != nil || len(raw) > 48<<20 {
		return nil, errors.New("native image response incomplete")
	}
	ref, err := sourcesNativeImageResult(ticket.Platform, raw)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(ref, "data:") {
		data, mime, err := p.owner.imageBytes(ctx, ref, 32<<20)
		if err != nil {
			return nil, errors.New("native image response invalid")
		}
		ref = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
	}
	return []string{ref}, nil
}
