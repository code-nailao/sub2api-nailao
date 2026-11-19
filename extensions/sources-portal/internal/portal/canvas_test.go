package portal

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestCanvasExistingGrantsAndUserIsolation(t *testing.T) {
	s := testServer(t, "user")
	var err error
	s.Studio, err = OpenStudio(t.TempDir(), s.SubOrigin, []StudioCapability{{GroupID: 26, Models: []string{"gpt-image-2"}, Engine: "sub2api"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	s.StudioURL = "https://studio.example.com/"
	s.CanvasURL = "https://canvas.example.com/canvas"
	user, creates := int64(9), 0
	revoked := false
	s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			return reply(200, fmt.Sprintf(`{"code":0,"data":{"id":%d,"role":"user","status":"active"}}`, user)), nil
		case "/api/v1/groups/available":
			return reply(200, `{"code":0,"data":[{"id":26,"name":"Images","platform":"openai","status":"active","allow_image_generation":true}]}`), nil
		case "/api/v1/keys":
			if r.Method == "POST" {
				creates++
				return reply(200, `{"code":0,"data":{"id":1,"user_id":9,"group_id":26,"key":"sk-canvas-fixture","status":"active"}}`), nil
			}
			return reply(200, `{"code":0,"data":{"items":[]}}`), nil
		case "/api/v1/keys/1":
			if revoked {
				return reply(404, `{}`), nil
			}
			return reply(200, `{"code":0,"data":{"id":1,"user_id":9,"group_id":26,"key":"sk-canvas-fixture","status":"active"}}`), nil
		}
		t.Errorf("unexpected native request: %s", r.URL.Path)
		return reply(404, `{}`), nil
	})
	call := func(body string) int {
		return request(s, "POST", "/api/canvas/connection", body, "fixture-user", s.PublicOrigin).Code
	}
	if call(`{"group_id":26,"restore_only":true}`) != 403 || creates != 0 {
		t.Fatal("restore created a new grant")
	}
	response := request(s, "POST", "/api/canvas/connection", `{"group_id":26}`, "fixture-user", s.PublicOrigin)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var config map[string]any
	if json.Unmarshal(response.Body.Bytes(), &config) != nil || config["surface"] != "media" || config["user_id"] != float64(9) || config["main_origin"] != s.SubOrigin || config["selectedConnectionId"] != "group-26" {
		t.Fatal("canvas identity contract missing")
	}
	if !strings.Contains(response.Body.String(), `"editPath":"/v1/images/edits/async"`) {
		t.Fatal("native edit not configured")
	}
	if call(`{"group_id":26,"restore_only":true}`) != 200 || creates != 1 {
		t.Fatal("existing grant not reused")
	}
	revoked = true
	if call(`{"group_id":26,"restore_only":true}`) != 403 || creates != 1 {
		t.Fatal("revoked key recreated")
	}
	revoked = false
	user = 10
	if call(`{"group_id":26,"restore_only":true}`) != 403 || creates != 1 {
		t.Fatal("another user's grant restored")
	}
	page := request(s, "GET", "/canvas", "", "", "")
	if page.Code != 200 || !strings.Contains(page.Header().Get("Content-Security-Policy"), "https://canvas.example.com") || !strings.Contains(page.Body.String(), "无限画布") {
		t.Fatal("canvas page or CSP missing")
	}
}
