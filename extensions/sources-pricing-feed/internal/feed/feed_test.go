package feed

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{SourceURL: "https://gateway.example/api/v1/model-plaza", SiteName: "Example", SiteDomain: "gateway.example", CNYPerCredit: "2",
		Quotes: []Quote{{GroupID: 10, GroupName: "stable-group", Models: []string{"gpt-example"}}},
	}
}

const testSource = `{"code":0,"data":{"groups":[{"id":10,"rate_multiplier":0.25,"is_exclusive":false,"models":[{"name":"gpt-example","pricing":{"billing_mode":"token","input_price":0.000002,"output_price":0.000008,"cache_read_price":0.0000002,"cache_write_price":null,"cache_write_1h_price":0,"intervals":[]}}]}]}}`

func source(t *testing.T, text string) plazaResponse {
	t.Helper()
	var out plazaResponse
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestConvertExactPricesAndStableNames(t *testing.T) {
	now := time.Now()
	r, err := convert(testConfig(), source(t, testSource), now)
	if err != nil {
		t.Fatal(err)
	}
	m := r.Data.Models[0]
	if m.Input != "1" || *m.Output != "4" || *m.CacheRead != "0.1" || m.CacheWrite != nil || *m.Cache1h != "0" {
		t.Fatalf("wrong conversion or null handling: %+v", m)
	}
	if m.Group != "stable-group" || m.Name != "gpt-example" || !m.Enabled || !r.Data.UpdatedAt.Equal(now) || r.Version != "1.1" || !r.Success {
		t.Fatal("wrong envelope")
	}
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), `"input_price":1`) || !strings.Contains(string(b), `"cache_create_price":null`) {
		t.Fatal(string(b))
	}
}

func TestConvertBaseContextTier(t *testing.T) {
	s := source(t, testSource)
	one, two := json.Number("0.000001"), json.Number("0.000004")
	s.Data.Groups[0].Models[0].Pricing.Intervals = []interval{{MinTokens: 1000, prices: prices{Input: &two}}, {MinTokens: 0, prices: prices{Input: &one}}}
	r, err := convert(testConfig(), s, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if r.Data.Models[0].Input != "0.5" || !strings.Contains(r.Data.Models[0].Note, "阶梯") {
		t.Fatal("not the base context price")
	}
}

func TestConvertFailuresKeepUnknownDistinctFromFree(t *testing.T) {
	cases := map[string]func(*plazaResponse){
		"missing envelope": func(s *plazaResponse) { s.Code = nil },
		"source error":     func(s *plazaResponse) { *s.Code = 401 },
		"missing group":    func(s *plazaResponse) { s.Data.Groups = nil },
		"exclusive":        func(s *plazaResponse) { s.Data.Groups[0].Exclusive = true },
		"personal price":   func(s *plazaResponse) { n := json.Number("1"); s.Data.Groups[0].UserRate = &n },
		"zero rate":        func(s *plazaResponse) { s.Data.Groups[0].Rate = "0" },
		"negative rate":    func(s *plazaResponse) { s.Data.Groups[0].Rate = "-1" },
		"missing model":    func(s *plazaResponse) { s.Data.Groups[0].Models = nil },
		"duplicate model": func(s *plazaResponse) {
			s.Data.Groups[0].Models = append(s.Data.Groups[0].Models, s.Data.Groups[0].Models[0])
		},
		"duplicate group":   func(s *plazaResponse) { s.Data.Groups = append(s.Data.Groups, s.Data.Groups[0]) },
		"missing input":     func(s *plazaResponse) { s.Data.Groups[0].Models[0].Pricing.Input = nil },
		"free input":        func(s *plazaResponse) { *s.Data.Groups[0].Models[0].Pricing.Input = "0" },
		"negative output":   func(s *plazaResponse) { *s.Data.Groups[0].Models[0].Pricing.Output = "-1" },
		"missing price":     func(s *plazaResponse) { s.Data.Groups[0].Models[0].Pricing = nil },
		"image unit":        func(s *plazaResponse) { s.Data.Groups[0].Models[0].Pricing.Mode = "image" },
		"group peak":        func(s *plazaResponse) { s.Data.Groups[0].Peak = true },
		"model time":        func(s *plazaResponse) { s.Data.Groups[0].Models[0].TimePricing = json.RawMessage(`{"periods":[{}]}`) },
		"missing base tier": func(s *plazaResponse) { s.Data.Groups[0].Models[0].Pricing.Intervals = []interval{{MinTokens: 1000}} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := source(t, testSource)
			mutate(&s)
			if _, err := convert(testConfig(), s, time.Now()); err == nil {
				t.Fatal("accepted invalid source")
			}
		})
	}
}

func TestConfigValidation(t *testing.T) {
	for _, raw := range []string{"", "-1", "NaN", "1e999999", "1e-999999", "1/2"} {
		c := testConfig()
		c.CNYPerCredit = json.Number(raw)
		if c.Validate() == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	for _, url := range []string{"http://gateway.example/api/v1/model-plaza", "https://user:pass@gateway.example/api/v1/model-plaza", "https://gateway.example/api/v1/model-plaza?token=x", "https://gateway.example/other"} {
		c := testConfig()
		c.SourceURL = url
		if c.Validate() == nil {
			t.Fatalf("accepted %q", url)
		}
	}
	c := testConfig()
	c.Quotes[0].Models = append(c.Quotes[0].Models, "gpt-example")
	if c.Validate() == nil {
		t.Fatal("duplicate model")
	}
	c = testConfig()
	c.Quotes = append(c.Quotes, c.Quotes[0])
	if c.Validate() == nil {
		t.Fatal("duplicate group")
	}
	c = testConfig()
	c.Quotes[0].Models = []string{"example-*"}
	if c.Validate() == nil {
		t.Fatal("wildcard")
	}
	c = testConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func makeService(t *testing.T, c Config, dir string) *Service {
	t.Helper()
	s, err := New(c, dir, quiet())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func request(s *Service) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/provider/pricing", nil))
	return w
}

func TestLiveFetchStalenessRecoveryAndNoCredentials(t *testing.T) {
	var fail atomic.Bool
	var hits atomic.Int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.URL.Path != "/api/v1/model-plaza" {
			t.Error("unexpected credential or path")
		}
		if fail.Load() {
			http.Error(w, "unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, testSource)
	}))
	defer ts.Close()
	c := testConfig()
	c.SourceURL = ts.URL + "/api/v1/model-plaza"
	s := makeService(t, c, t.TempDir())
	s.client = ts.Client()
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	s.now = func() time.Time { return time.Unix(0, clock.Load()) }
	if request(s).Code != 503 {
		t.Fatal("cold start served prices")
	}
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := request(s)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code)
	}
	body := w.Body.String()
	fail.Store(true)
	clock.Add(int64(20 * time.Minute))
	if err := s.Sync(context.Background()); err == nil {
		t.Fatal("source failure accepted")
	}
	if next := request(s); next.Code != 200 || next.Body.String() != body {
		t.Fatal("last success timestamp was rewritten")
	}
	clock.Add(int64(11 * time.Minute))
	if request(s).Code != 503 {
		t.Fatal("stale prices served")
	}
	fail.Store(false)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if request(s).Code != 200 {
		t.Fatal("did not recover")
	}
	if hits.Load() != 3 {
		t.Fatal("public requests triggered upstream fetch")
	}
}

func TestSnapshotRestartsAndConfigInvalidatesCache(t *testing.T) {
	c := testConfig()
	dir := t.TempDir()
	s, err := New(c, dir, quiet())
	if err != nil {
		t.Fatal(err)
	}
	r, err := convert(c, source(t, testSource), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.persist(snapshot{Config: c.fingerprint(), Response: r}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(c, dir, quiet()); err == nil {
		t.Fatal("second process acquired lock")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := New(c, dir, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if request(restored).Code != 200 {
		t.Fatal("valid cache did not restore")
	}
	restored.Close()
	c.CNYPerCredit = "3"
	changed := makeService(t, c, dir)
	if request(changed).Code != 503 {
		t.Fatal("old config price restored")
	}
}

func TestCorruptAndFutureSnapshotsAreNotServed(t *testing.T) {
	for _, kind := range []string{"corrupt", "missing-model", "future", "expired"} {
		t.Run(kind, func(t *testing.T) {
			c := testConfig()
			dir := t.TempDir()
			s, err := New(c, dir, quiet())
			if err != nil {
				t.Fatal(err)
			}
			r, _ := convert(c, source(t, testSource), time.Now())
			switch kind {
			case "missing-model":
				r.Data.Models = nil
			case "future":
				r.Data.UpdatedAt = time.Now().Add(time.Hour)
			case "expired":
				r.Data.UpdatedAt = time.Now().Add(-MaxStale - time.Second)
			}
			if err := s.persist(snapshot{Config: c.fingerprint(), Response: r}); err != nil {
				t.Fatal(err)
			}
			s.Close()
			if kind == "corrupt" {
				if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), []byte(`{`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			restored := makeService(t, c, dir)
			if request(restored).Code != 503 {
				t.Fatal("invalid snapshot served")
			}
		})
	}
}

func TestConcurrentRefreshAndRead(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, testSource) }))
	defer ts.Close()
	c := testConfig()
	c.SourceURL = ts.URL + "/api/v1/model-plaza"
	s := makeService(t, c, t.TempDir())
	s.client = ts.Client()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Sync(context.Background()); err != nil {
				t.Error(err)
			}
			if request(s).Code != 200 {
				t.Error("no snapshot")
			}
		}()
	}
	wg.Wait()
}

func TestRedirectAndInvalidJSONDoNotReplaceSnapshot(t *testing.T) {
	var mode atomic.Int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 1:
			http.Redirect(w, r, "/other", 302)
		case 2:
			io.WriteString(w, `<html>login</html>`)
		default:
			io.WriteString(w, testSource)
		}
	}))
	defer ts.Close()
	c := testConfig()
	c.SourceURL = ts.URL + "/api/v1/model-plaza"
	s := makeService(t, c, t.TempDir())
	s.client.Transport = ts.Client().Transport
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := request(s).Body.String()
	for _, m := range []int32{1, 2} {
		mode.Store(m)
		if err := s.Sync(context.Background()); err == nil {
			t.Fatal("invalid source accepted")
		}
		if request(s).Body.String() != before {
			t.Fatal("snapshot changed")
		}
	}
}

func TestUnsupportedPricingSemantics(t *testing.T) {
	for _, model := range []string{"deepseek-v4-flash", "gpt-image-2.6-flare", "gpt-audio", "gpt-realtime"} {
		c := testConfig()
		c.Quotes[0].Models[0] = model
		if c.Validate() == nil {
			t.Fatalf("accepted unsupported model %s", model)
		}
		s := source(t, testSource)
		s.Data.Groups[0].Models[0].Name = model
		if _, err := convert(c, s, time.Now()); err == nil {
			t.Fatalf("converted unsupported model %s", model)
		}
	}
	for _, fields := range []string{`,"image_output_price":0.00003`, `,"reasoning_effort_multipliers":{"high":2}`} {
		s := source(t, strings.Replace(testSource, `"intervals":[]`, `"intervals":[]`+fields, 1))
		if _, err := convert(testConfig(), s, time.Now()); err == nil {
			t.Fatalf("accepted unrepresented charges: %s", fields)
		}
	}
	s := source(t, strings.Replace(testSource, `"intervals":[]`, `"intervals":[],"reasoning_effort_multipliers":{"high":1}`, 1))
	if _, err := convert(testConfig(), s, time.Now()); err != nil {
		t.Fatal(err)
	}
}
