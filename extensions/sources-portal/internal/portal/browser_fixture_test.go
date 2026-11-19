package portal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// 仅编译进测试二进制；正常服务没有mock模式或鉴权绕过开关。
func TestBrowserFixture(t *testing.T) {
	address := os.Getenv("PORTAL_BROWSER_LISTEN")
	if address == "" {
		t.Skip("explicit loopback browser fixture only")
	}
	if !strings.HasPrefix(address, "127.0.0.1:") {
		t.Fatal("fixture must bind loopback")
	}
	s := testServer(t, "admin")
	if os.Getenv("PORTAL_BROWSER_INTELLIGENCE") == "1" {
		s = intelligenceBrowserFixture(t)
	}
	var err error
	if os.Getenv("PORTAL_BROWSER_SEED_BALANCE") == "1" {
		in := input()
		in.Name = "本地钱包 fixture"
		source, saveErr := s.Service.Save("", in, 9)
		if saveErr != nil {
			t.Fatal(saveErr)
		}
		if _, err = s.Service.Refresh(context.Background(), source.ID); err != nil {
			t.Fatal(err)
		}
	}
	s.InternalSubOrigin = s.SubOrigin
	s.PublicOrigin = "http://" + address
	// This test-only fixture accepts one named acceptance host over HTTPS or a
	// loopback proxy serving the actual Main build. The host comes from the
	// environment so no deployment is named here. Production origin checks stay
	// intact.
	if raw := os.Getenv("PORTAL_BROWSER_ORIGIN"); raw != "" {
		u, parseErr := url.Parse(raw)
		acceptanceHost := os.Getenv("PORTAL_BROWSER_ACCEPTANCE_HOST")
		allowed := parseErr == nil && u.Scheme == "https" && acceptanceHost != "" && u.Host == acceptanceHost
		if parseErr == nil && u.Scheme == "http" && u.Hostname() == "127.0.0.1" {
			port, portErr := strconv.Atoi(u.Port())
			allowed = portErr == nil && port > 0 && port <= 65535 && u.Host == "127.0.0.1:"+u.Port()
		}
		if !allowed || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			t.Fatal("fixture origin must be the fixed acceptance origin or an HTTP loopback port")
		}
		s.PublicOrigin = raw
	}
	s.SubOrigin = s.PublicOrigin
	if homeDir := os.Getenv("PORTAL_HOME_DIR"); homeDir != "" {
		s.Content, err = OpenContentStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		s.HomeFiles = os.DirFS(homeDir)
	}
	if studioURL := os.Getenv("PORTAL_BROWSER_STUDIO_URL"); studioURL != "" || os.Getenv("PORTAL_BROWSER_CANVAS_URL") != "" {
		s.Studio, err = OpenStudio(t.TempDir(), "https://fixture-api.example.com", []StudioCapability{{GroupID: 26, Engine: "sub2api", Models: []string{"gpt-image-1"}}, {GroupID: 74, Engine: "sub2api", Models: []string{"gpt-image-1"}}}, "")
		if err != nil {
			t.Fatal(err)
		}
		s.StudioURL = studioURL
		s.CanvasURL = os.Getenv("PORTAL_BROWSER_CANVAS_URL")
		var mu sync.Mutex
		keys := map[int64]studioKey{}
		count := int64(0)
		browserToken := "fixture." + base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":9,"sid":"browser-fixture","version":2}`)) + ".fixture"
		otherToken := "fixture." + base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":10,"sid":"other-fixture","version":2}`)) + ".fixture"
		s.AuthClient.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			userID := int64(9)
			role := "admin"
			if r.Header.Get("Authorization") == "Bearer "+otherToken {
				userID = 10
				role = "user"
			}
			switch r.URL.Path {
			case "/api/v1/auth/me":
				if r.Header.Get("Authorization") != "Bearer fixture-admin" && r.Header.Get("Authorization") != "Bearer "+browserToken && r.Header.Get("Authorization") != "Bearer "+otherToken {
					return reply(401, ""), nil
				}
				return reply(200, fmt.Sprintf(`{"code":0,"data":{"id":%d,"role":%q,"status":"active","username":"验收账户","balance":12.50}}`, userID, role)), nil
			case "/api/v1/admin/accounts/balance-identities":
				if role != "admin" {
					return reply(403, ""), nil
				}
				ids, parseErr := parseBalanceIDs(r.URL.Query().Get("ids"))
				if r.Method != "GET" || parseErr != nil {
					return reply(400, ""), nil
				}
				identities := []BalanceAccountIdentity{}
				for _, id := range ids {
					if id == 1 || id == 2 {
						identities = append(identities, BalanceAccountIdentity{AccountID: id, Supported: true, Marker: "v1:" + strings.Repeat("a", 64)})
					}
				}
				payload, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"accounts": identities}})
				return reply(200, string(payload)), nil
			case "/api/v1/groups/available":
				return reply(200, `{"code":0,"data":[{"id":26,"name":"普通图片 A","status":"active","platform":"openai","allow_image_generation":true},{"id":74,"name":"普通图片 B","status":"active","platform":"openai","allow_image_generation":true}]}`), nil
			case "/api/v1/keys":
				if r.Method == "POST" {
					var in struct {
						Name    string `json:"name"`
						GroupID int64  `json:"group_id"`
					}
					_ = json.NewDecoder(r.Body).Decode(&in)
					count++
					key := studioKey{ID: count, UserID: userID, GroupID: in.GroupID, Name: in.Name, Key: fmt.Sprintf("sk-studio-fixture-%d-%d", userID, in.GroupID), Status: "active"}
					keys[count] = key
					b, _ := json.Marshal(key)
					return reply(200, `{"code":0,"data":`+string(b)+`}`), nil
				}
				return reply(200, `{"code":0,"data":{"items":[]}}`), nil
			default:
				var id int64
				_, _ = fmt.Sscanf(r.URL.Path, "/api/v1/keys/%d", &id)
				key, ok := keys[id]
				if !ok {
					return reply(404, ""), nil
				}
				if r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/name") {
					var in struct {
						Name         string `json:"name"`
						ExpectedName string `json:"expected_name"`
						GroupID      int64  `json:"expected_group_id"`
					}
					if json.NewDecoder(r.Body).Decode(&in) != nil {
						return reply(400, ""), nil
					}
					if key.UserID != userID || key.Name != in.ExpectedName || key.GroupID != in.GroupID {
						return reply(409, ""), nil
					}
					key.Name = html.EscapeString(in.Name)
					keys[id] = key
					b, _ := json.Marshal(map[string]any{"id": id, "name": key.Name})
					return reply(200, `{"code":0,"data":`+string(b)+`}`), nil
				}
				b, _ := json.Marshal(key)
				return reply(200, `{"code":0,"data":`+string(b)+`}`), nil
			}
		})
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	if os.Getenv("PORTAL_BROWSER_INTELLIGENCE") == "1" {
		handler = intelligenceFixtureHandler(handler)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	var workers sync.WaitGroup
	if s.Intelligence != nil {
		workers.Add(1)
		go func() { defer workers.Done(); s.Intelligence.Run(ctx) }()
	}
	defer workers.Wait()
	go func() { <-ctx.Done(); _ = server.Close() }()
	if err = server.Serve(listener); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
