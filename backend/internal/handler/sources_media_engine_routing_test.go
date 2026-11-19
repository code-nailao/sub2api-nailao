package handler

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Task lookups reach the media engine only for ids with one of its prefixes;
// everything else goes to the ordinary async image path, which reports that
// async tasks are disabled in this fixture. The legacy values are fake.
func TestAsyncImageGetRoutesMediaEngineTaskIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { service.SetSourcesMediaEngineLegacy("", "") })
	routed := func(id string) bool {
		out := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(out)
		c.Request = httptest.NewRequest("GET", "/v1/images/tasks/"+id, nil)
		c.Params = gin.Params{{Key: "task_id", Value: id}}
		(&AsyncImageHandler{}).Get(c)
		return !strings.Contains(out.Body.String(), "async image tasks are not enabled")
	}
	hex := strings.Repeat("a", 32)
	for _, tc := range []struct {
		name, prefix string
		want         map[string]bool
	}{
		{"", "", map[string]bool{"mediatask_" + hex: true, "legacytask_" + hex: false, "img_" + hex: false}},
		{"legacyname", "legacytask", map[string]bool{"mediatask_" + hex: true, "legacytask_" + hex: true, "othertask_" + hex: false, "img_" + hex: false}},
	} {
		service.SetSourcesMediaEngineLegacy(tc.name, tc.prefix)
		for id, want := range tc.want {
			if got := routed(id); got != want {
				t.Fatalf("legacy %q: %s routed=%v", tc.name, id, got)
			}
		}
	}
}
