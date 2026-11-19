package portal

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestStudioNativeGeminiConnection(t *testing.T) {
	s := testServer(t, "user")
	s.CanvasURL = "https://canvas.example.com/canvas"
	var err error
	s.Studio, err = OpenStudio(t.TempDir(), s.SubOrigin, []StudioCapability{
		{GroupID: 243, Engine: "sub2api", Models: []string{"gemini-3.1-flash-image-preview", "gemini-3-pro-image-preview"}},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	creates := 0
	s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			return reply(200, `{"code":0,"data":{"id":9,"role":"user","status":"active"}}`), nil
		case "/api/v1/groups/available":
			return reply(200, `{"code":0,"data":[{"id":243,"name":"Gemini Image（AI Studio）","platform":"gemini","status":"active","allow_image_generation":true},{"id":59,"name":"Unregistered Gemini","platform":"gemini","status":"active","allow_image_generation":true}]}`), nil
		case "/api/v1/keys":
			if r.Method == http.MethodPost {
				creates++
				return reply(200, `{"code":0,"data":{"id":42,"user_id":9,"group_id":243,"status":"active","key":"fixture-gemini-key"}}`), nil
			}
			return reply(200, `{"code":0,"data":{"items":[]}}`), nil
		case "/api/v1/keys/42":
			return reply(200, `{"code":0,"data":{"id":42,"user_id":9,"group_id":243,"status":"active","key":"fixture-gemini-key"}}`), nil
		default:
			t.Fatalf("unexpected Main API: %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})
	groups := request(s, "GET", "/api/studio/groups", "", "fixture-user", s.PublicOrigin)
	if groups.Code != http.StatusOK || !strings.Contains(groups.Body.String(), `"id":243`) || strings.Contains(groups.Body.String(), `"id":59`) {
		t.Fatalf("registered native Gemini group missing or unregistered group leaked: %d %s", groups.Code, groups.Body.String())
	}
	for _, path := range []string{"/api/studio/connection", "/api/canvas/connection"} {
		response := request(s, "POST", path, `{"group_id":243}`, "fixture-user", s.PublicOrigin)
		if response.Code != http.StatusOK {
			t.Fatalf("native Gemini authorization failed: %d %s", response.Code, response.Body.String())
		}
		var payload struct {
			Connections  []struct{ Protocol string }
			Capabilities []struct {
				Protocol string
				Task     struct {
					Engine, CreatePath, EditPath, StatusPathTemplate string
					Modes                                            []string
				}
				Models []struct {
					Parameters []struct {
						Key       string
						Supported bool
					}
				}
			}
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Connections) != 1 || len(payload.Capabilities) != 1 {
			t.Fatal(response.Body.String())
		}
		capability := payload.Capabilities[0]
		if payload.Connections[0].Protocol != "gemini_native" || capability.Protocol != "gemini_native" {
			t.Fatalf("Gemini advertised with wrong protocol: %s", response.Body.String())
		}
		if capability.Task.Engine != "sub2api" || strings.Join(capability.Task.Modes, ",") != "sync" || capability.Task.CreatePath != "/v1beta/models/{model}:generateContent" || capability.Task.EditPath != capability.Task.CreatePath || capability.Task.StatusPathTemplate != "" {
			t.Fatalf("Gemini advertised as an OpenAI async task: %+v", capability.Task)
		}
		for _, model := range capability.Models {
			params := map[string]bool{}
			for _, parameter := range model.Parameters {
				params[parameter.Key] = parameter.Supported
			}
			if !params["size"] || !params["n"] || params["mask"] || params["quality"] || params["stream"] {
				t.Fatalf("wrong native Gemini parameter contract: %+v", params)
			}
		}
	}
	if creates != 1 {
		t.Fatalf("Studio/Canvas did not reuse the ordinary group key: %d creates", creates)
	}
}
