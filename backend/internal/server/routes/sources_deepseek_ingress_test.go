package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newDeepSeekIngressRouter(t *testing.T, runMode string, cache service.BillingCache) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{RunMode: runMode, Gateway: config.GatewayConfig{MaxBodySize: 128 << 20, TextMaxBodySize: 1 << 20}}
	billing := service.NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	gateway := handler.NewOpenAIGatewayHandler(nil, nil, billing, nil, nil, nil, nil, nil, cfg)
	group := allowlistGroup(service.PlatformDeepseek, false)
	price := float64(1)
	group.SearchPricePer1k = &price
	group.DeepSeekTools = service.DeepSeekToolsConfig{
		Enabled: true, FetchPricePer1K: &price, OCRPricePer1KPages: &price,
		ASRPricePerHour: &price, TTSPricePer10KChars: &price,
	}
	router := gin.New()
	RegisterGatewayRoutes(router, &handler.Handlers{
		Gateway: &handler.GatewayHandler{}, OpenAIGateway: gateway,
		AsyncImage: handler.NewAsyncImageHandler(nil, nil),
	}, middleware.APIKeyAuthMiddleware(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
			GroupID: &group.ID, Group: group, User: &service.User{ID: 1},
		})
		// No authenticated subject or supplier is needed to test the ingress and
		// local upload parsing before moderation, scheduling or settlement.
		c.Next()
	}), nil, nil, nil, nil, nil, cfg)
	return router
}

func writeDeepSeekIngressUpload(t *testing.T, name string, data []byte, repeated bool) (string, *bytes.Buffer) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if name == "note.pdf" {
		require.NoError(t, writer.WriteField("purpose", "text_recognition"))
	}
	part, err := writer.CreateFormFile("file", name)
	require.NoError(t, err)
	_, err = part.Write(data)
	require.NoError(t, err)
	if repeated {
		part, err = writer.CreateFormFile("file", name)
		require.NoError(t, err)
		_, err = part.Write(data)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return writer.FormDataContentType(), &body
}

func TestDeepSeekIngressRejectsDuplicateFieldsAcrossToolRoutes(t *testing.T) {
	router := newDeepSeekIngressRouter(t, config.RunModeSimple, nil)
	for _, tc := range []struct{ tool, field, json, filename string }{
		{tool: "search", field: "query", json: `{"query":"test","query":"test"}`},
		{tool: "fetch", field: "url", json: `{"url":"","url":""}`},
		{tool: "tts", field: "text", json: `{"text":"test","text":"test"}`},
		{tool: "ocr", field: "file", filename: "note.pdf"},
		{tool: "asr", field: "file", filename: "speech.wav"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			var body io.Reader = strings.NewReader(tc.json)
			contentType := "application/json"
			if tc.filename != "" {
				contentType, body = writeDeepSeekIngressUpload(t, tc.filename, []byte("fixture"), true)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/tools/"+tc.tool, body)
			req.Header.Set("Content-Type", contentType)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			var response struct {
				Error struct{ Type, Message string }
			}
			dec := json.NewDecoder(w.Body)
			require.NoError(t, dec.Decode(&response))
			require.Equal(t, "invalid_request_error", response.Error.Type)
			require.Contains(t, response.Error.Message, `"`+tc.field+`"`)
			require.ErrorIs(t, dec.Decode(&response), io.EOF, "the router writes one rejection")
		})
	}
}

func TestDeepSeekIngressPreservesValidUploads(t *testing.T) {
	router := newDeepSeekIngressRouter(t, config.RunModeSimple, nil)
	for _, tc := range []struct{ tool, filename, data string }{
		{"ocr", "note.pdf", "%PDF-1.4\n"},
		{"asr", "speech.wav", "RIFF\x28\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x40\x1f\x00\x00\x80\x3e\x00\x00\x02\x00\x10\x00data\x04\x00\x00\x00\x00\x00\x00\x00"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			contentType, body := writeDeepSeekIngressUpload(t, tc.filename, []byte(tc.data), false)
			req := httptest.NewRequest(http.MethodPost, "/v1/tools/"+tc.tool, body)
			req.Header.Set("Content-Type", contentType)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), "user required", "the handler reached the subject check after parsing the upload")
		})
	}
}

type deepSeekIngressEmptyBalance struct{ service.BillingCache }

func (deepSeekIngressEmptyBalance) GetUserBalance(context.Context, int64) (float64, error) {
	return 0, nil
}

type deepSeekIngressWatchedBody struct{ reads int }

func (b *deepSeekIngressWatchedBody) Read([]byte) (int, error) {
	b.reads++
	return 0, io.EOF
}

func TestDeepSeekIngressRejectsUnfundedUploadBeforeReading(t *testing.T) {
	router := newDeepSeekIngressRouter(t, config.RunModeStandard, deepSeekIngressEmptyBalance{})
	body := &deepSeekIngressWatchedBody{}
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/ocr", body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=fixture")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.Zero(t, body.reads)
}

func TestDeepSeekIngressAppliesToolBodyLimitBeforeValidation(t *testing.T) {
	router := newDeepSeekIngressRouter(t, config.RunModeSimple, nil)
	body := `{"query":"test","query":"test"}` + strings.Repeat(" ", int(service.DeepSeekToolMaxBody("search")))
	req := httptest.NewRequest(http.MethodPost, "/v1/tools/search", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, w.Body.String())
}

func TestDeepSeekChatIngressRejectsOnceBeforeToolHandling(t *testing.T) {
	router := newGatewayRoutesTestRouterWithGroup(allowlistGroup(service.PlatformDeepseek, false))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"fixture","tools":[],"tools":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	var response struct {
		Error struct{ Type, Message string }
	}
	dec := json.NewDecoder(w.Body)
	require.NoError(t, dec.Decode(&response))
	require.Equal(t, "invalid_request_error", response.Error.Type)
	require.Contains(t, response.Error.Message, `"tools"`)
	require.ErrorIs(t, dec.Decode(&response), io.EOF)
}
