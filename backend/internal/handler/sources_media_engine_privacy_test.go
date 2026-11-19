package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"sources.local/mediaengine"
)

func TestAsyncImageErrorsDoNotExposeImplementation(t *testing.T) {
	_, _, invalid := service.SourcesMediaEngineAuditInput("/v1/images/generations/async", "application/json", []byte(`{"model":"unsupported-model","prompt":"test"}`))
	if invalid == nil {
		t.Fatal("invalid model unexpectedly accepted")
	}
	for _, err := range []error{fmt.Errorf("invalid media engine request: %w", invalid), errors.New("media engine https://private-provider.invalid credential unavailable"), errors.New("legacyname https://private-provider.invalid credential unavailable")} {
		out := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(out)
		sourcesImageError(c, err)
		for _, forbidden := range []string{"media engine", "media-engine", "legacyname", "upstream", "provider", "private-provider.invalid", "credential"} {
			if strings.Contains(strings.ToLower(out.Body.String()), forbidden) {
				t.Fatalf("public error leaks implementation: %s", out.Body)
			}
		}
	}
}
func TestAsyncImagePublicTaskDoesNotExposeProviderFailures(t *testing.T) {
	for _, phase := range []string{"failed", "unknown", "delivery_review"} {
		for _, failure := range []string{"upstream_failed", "provider_rejected", "media engine https://private-provider.invalid", "legacyname https://private-provider.invalid"} {
			raw, _ := json.Marshal(sourcesPublicImageTask(&mediaengine.Task{ID: "image-test", Phase: phase, Failure: failure}))
			for _, forbidden := range []string{"media engine", "media-engine", "legacyname", "upstream", "provider", "https://"} {
				if strings.Contains(strings.ToLower(string(raw)), forbidden) {
					t.Fatalf("public task leaks implementation: %s", raw)
				}
			}
		}
	}
}
