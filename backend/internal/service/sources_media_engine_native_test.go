package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sources.local/mediaengine"
)

func TestSourcesNativeImageRequestUsesImageProtocols(t *testing.T) {
	body := json.RawMessage(`{"model":"gpt-image-2","prompt":"test","size":"1024x1024","quality":"high","output_format":"png","images":["https://owned.example/input"]}`)
	path, out, err := sourcesNativeImageRequest(PlatformOpenAI, "1K", body, nil)
	require.NoError(t, err)
	require.Equal(t, "/v1/images/edits", path)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(out, &payload))
	require.Equal(t, "1024x1024", payload["size"])
	require.Equal(t, "high", payload["quality"])
	require.Equal(t, "1K", payload["output_resolution"])
	require.Equal(t, "b64_json", payload["response_format"])
	require.Equal(t, float64(1), payload["n"])
	require.NotContains(t, path, "videos")
	calls := 0
	path, out, err = sourcesNativeImageRequest(PlatformGemini, "1K", json.RawMessage(`{"model":"gemini-3.1-flash-image-preview","prompt":"test","size":"1376x768","images":["https://owned.example/input"]}`), func(ref string) ([]byte, string, error) {
		calls++
		require.Equal(t, "https://owned.example/input", ref)
		return []byte("fixture"), "image/png", nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Equal(t, "/v1beta/models/gemini-3.1-flash-image-preview:generateContent", path)
	require.NoError(t, json.Unmarshal(out, &payload))
	var native map[string]any
	require.NoError(t, json.Unmarshal(out, &native))
	require.NotContains(t, native, "prompt")
	require.NotContains(t, native, "images")
	require.NotContains(t, native, "response_format")
	require.JSONEq(t, `{"contents":[{"role":"user","parts":[{"text":"test"},{"inlineData":{"mimeType":"image/png","data":"Zml4dHVyZQ=="}}]}],"generationConfig":{"responseModalities":["TEXT","IMAGE"],"imageConfig":{"aspectRatio":"16:9","imageSize":"1K"}}}`, string(out))
}
func TestSourcesNativeImageInputFailureDefersOnlyBeforeGeneration(t *testing.T) {
	_, _, err := sourcesNativeImageRequest(PlatformGemini, "2K", json.RawMessage(`{"model":"gemini-3-pro-image-preview","prompt":"test","images":["https://owned.example/input"]}`), func(string) ([]byte, string, error) { return nil, "", errors.New("storage unavailable") })
	require.ErrorIs(t, err, mediaengine.ErrDeferred)
	for _, raw := range []string{`{"model":"../secret","prompt":"test"}`, `{"model":"gemini-3-pro-image-preview","prompt":"test","size":"1000x100"}`, `{"model":"gemini-3-pro-image-preview","prompt":"test","aspect_ratio":"unsupported"}`} {
		_, _, err = sourcesNativeImageRequest(PlatformGemini, "2K", json.RawMessage(raw), nil)
		require.ErrorIs(t, err, mediaengine.ErrRejected)
	}
}
func TestSourcesNativeImageResultCountsActualImages(t *testing.T) {
	ref, err := sourcesNativeImageResult(PlatformOpenAI, []byte(`{"data":[{"b64_json":"aW1hZ2U="}]}`))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(ref, "data:image/png;base64,"))
	ref, err = sourcesNativeImageResult(PlatformGemini, []byte(`{"candidates":[{"content":{"parts":[{"text":"done"},{"inlineData":{"mimeType":"image/webp","data":"aW1hZ2U="}}]}}]}`))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(ref, "data:image/webp;base64,"))
	for _, raw := range []string{`{"data":[{}, {"url":"https://owned.example/1"}]}`, `{"data":[{"url":"https://owned.example/1"},{"url":"https://owned.example/2"}]}`, `{"data":[{"url":"file:///secret"}]}`, `{"error":{"message":"internal provider failure"}}`, `{"id":"task_unknown","status":"queued"}`} {
		_, err = sourcesNativeImageResult(PlatformOpenAI, []byte(raw))
		require.Error(t, err)
	}
}
