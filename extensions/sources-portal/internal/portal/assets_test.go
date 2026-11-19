package portal

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHomeAssetDraftPrivacyAndPublication(t *testing.T) {
	s := testServer(t, "admin")
	s.Content, _ = OpenContentStore(t.TempDir())
	s.HomeFiles = webFiles
	var data bytes.Buffer
	if e := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2))); e != nil {
		t.Fatal(e)
	}
	upload := func(token string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/home/assets", bytes.NewReader(body))
		r.Header.Set("Content-Type", "image/png")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if w := upload("", data.Bytes()); w.Code != 401 {
		t.Fatal("anonymous upload", w.Code)
	}
	if w := upload("fixture-admin", []byte("<html>not an image")); w.Code != 400 {
		t.Fatal("bad image", w.Code)
	}
	w := upload("fixture-admin", data.Bytes())
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var response struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &response)
	if !strings.HasPrefix(response.Path, "/content-assets/") {
		t.Fatal(response.Path)
	}
	if w = request(s, "GET", response.Path, "", "", ""); w.Code != 404 {
		t.Fatal("draft image leaked", w.Code)
	}
	adminPath := "/api/home/assets/" + strings.TrimPrefix(response.Path, "/content-assets/")
	if w = request(s, "GET", adminPath, "", "", ""); w.Code != 401 {
		t.Fatal("admin image exposed")
	}
	if w = request(s, "GET", adminPath, "", "fixture-admin", ""); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data.Bytes()) {
		t.Fatal("preview failed")
	}
	state := s.Content.Read()
	c := state.Draft
	c.LogoLight = response.Path
	next, e := s.Content.Change("draft", state.Revision, 0, 9, &c)
	if e != nil {
		t.Fatal(e)
	}
	if w = request(s, "GET", response.Path, "", "", ""); w.Code != 404 {
		t.Fatal("draft reference leaked")
	}
	if _, e = s.Content.Change("publish", next.Revision, 0, 9, nil); e != nil {
		t.Fatal(e)
	}
	if w = request(s, "GET", response.Path, "", "", ""); w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatal("publication failed", w.Code)
	}
}
