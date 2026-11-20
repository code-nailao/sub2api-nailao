package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewManager_PreservesOrder(t *testing.T) {
	configs := []ProviderConfig{
		{Type: "brave", APIKey: "k3"},
		{Type: "tavily", APIKey: "k1"},
	}
	m := NewManager(configs, nil)
	require.Equal(t, "brave", m.configs[0].Type)
	require.Equal(t, "tavily", m.configs[1].Type)
}

func TestManager_SearchWithBestProvider_EmptyQuery(t *testing.T) {
	m := NewManager([]ProviderConfig{{Type: "brave", APIKey: "k"}}, nil)
	_, _, err := m.SearchWithBestProvider(context.Background(), SearchRequest{Query: ""})
	require.ErrorContains(t, err, "empty search query")

	_, _, err = m.SearchWithBestProvider(context.Background(), SearchRequest{Query: "   "})
	require.ErrorContains(t, err, "empty search query")
}

func TestManager_SearchWithBestProvider_SkipEmptyAPIKey(t *testing.T) {
	m := NewManager([]ProviderConfig{{Type: "brave", APIKey: ""}}, nil)
	_, _, err := m.SearchWithBestProvider(context.Background(), SearchRequest{Query: "test"})
	require.ErrorContains(t, err, "no available provider")
}

func TestManager_SearchWithBestProvider_SkipExpired(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour).Unix()
	m := NewManager([]ProviderConfig{
		{Type: "brave", APIKey: "k", ExpiresAt: &past},
	}, nil)
	_, _, err := m.SearchWithBestProvider(context.Background(), SearchRequest{Query: "test"})
	require.ErrorContains(t, err, "no available provider")
}

func TestManager_SearchWithBestProvider_UsesFirstAvailable(t *testing.T) {
	srvBrave := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := braveResponse{}
		resp.Web.Results = []braveResult{{URL: "https://brave.com", Title: "Brave", Description: "from brave"}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srvBrave.Close()

	origURL := *braveSearchURL
	u, _ := http.NewRequest("GET", srvBrave.URL, nil)
	*braveSearchURL = *u.URL
	defer func() { *braveSearchURL = origURL }()

	m := NewManager([]ProviderConfig{
		{Type: "brave", APIKey: "k1"},
		{Type: "tavily", APIKey: "k2"},
	}, nil)
	m.clientCache[srvBrave.URL] = srvBrave.Client()
	m.clientCache[""] = srvBrave.Client()

	resp, providerName, err := m.SearchWithBestProvider(context.Background(), SearchRequest{Query: "test"})
	require.NoError(t, err)
	require.Equal(t, "brave", providerName)
	require.Len(t, resp.Results, 1)
	require.Equal(t, "from brave", resp.Results[0].Snippet)
}

func TestManagerTestSearchOnlyFallsBackBeforeSubmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		sent   bool
	}{
		{name: "408", status: http.StatusRequestTimeout, sent: true},
		{name: "disconnected-after-write", sent: true},
		{name: "confirmed-preconnect-failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager([]ProviderConfig{{Type: ProviderTypeExa, APIKey: "fixture-first"}, {Type: ProviderTypeTavily, APIKey: "fixture-backup"}}, nil)
			calls := 0
			manager.clientCache[""] = &http.Client{Transport: exaRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					trace := httptrace.ContextClientTrace(req.Context())
					trace.GetConn(req.URL.Host)
					if tc.sent {
						trace.WroteRequest(httptrace.WroteRequestInfo{})
					}
					if tc.status == 0 {
						return nil, io.ErrUnexpectedEOF
					}
					return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: http.NoBody}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"results":[]}`))}, nil
			})}

			result, provider, err := manager.TestSearch(t.Context(), SearchRequest{Query: "fixture"})

			if tc.sent {
				require.Equal(t, 1, calls, "admin search must not reach the backup after an ambiguous submission")
				require.ErrorIs(t, err, ErrSearchOutcomeUnknown)
				require.Nil(t, result)
				require.Empty(t, provider)
			} else {
				require.NoError(t, err)
				require.Equal(t, 2, calls)
				require.Equal(t, ProviderTypeTavily, provider)
			}
		})
	}
}

func TestManagerDoesNotReplaySearchAfterHTTP408(t *testing.T) {
	for _, first := range []string{"exa", "brave", "tavily"} {
		t.Run(first, func(t *testing.T) {
			backup := "brave"
			if first == backup {
				backup = "exa"
			}
			manager := NewManager([]ProviderConfig{{Type: first, APIKey: "fixture-first"}, {Type: backup, APIKey: "fixture-backup"}}, nil)
			var calls []string
			manager.clientCache[""] = &http.Client{Transport: exaRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls = append(calls, req.URL.Hostname())
				if len(calls) == 1 {
					return &http.Response{StatusCode: http.StatusRequestTimeout, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"message":"fixture timeout"}`))}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})}

			result, _, err := manager.SearchWithBestProvider(t.Context(), SearchRequest{Query: "fixture"})

			require.Len(t, calls, 1, "a 408 must not submit the same paid search to a second provider")
			require.Nil(t, result)
			require.ErrorIs(t, err, ErrSearchOutcomeUnknown)
			require.ErrorContains(t, err, "408")
		})
	}
}

func TestManagerOnlyFallsBackBeforeAnySearchCouldBeSent(t *testing.T) {
	cases := []struct {
		name       string
		send       bool
		status     int
		body       string
		writeError error
	}{
		{name: "confirmed-preconnect-timeout"},
		{name: "timeout-after-write", send: true},
		{name: "ambiguous-partial-write", send: true, writeError: io.ErrShortWrite},
		{name: "provider-error-response", send: true, status: http.StatusServiceUnavailable, body: `{}`},
		{name: "broken-response", send: true, status: http.StatusOK, body: `{`},
	}
	for _, first := range []string{"exa", "brave", "tavily"} {
		for _, tc := range cases {
			t.Run(first+"/"+tc.name, func(t *testing.T) {
				backup := "tavily"
				if first == backup {
					backup = "exa"
				}
				manager := NewManager([]ProviderConfig{{Type: first, APIKey: "fixture-first"}, {Type: backup, APIKey: "fixture-backup"}}, nil)
				calls := 0
				manager.clientCache[""] = &http.Client{Transport: exaRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					trace := httptrace.ContextClientTrace(req.Context())
					if trace != nil && trace.GetConn != nil {
						trace.GetConn(req.URL.Host)
					}
					if calls == 1 {
						if tc.send && trace != nil && trace.WroteRequest != nil {
							trace.WroteRequest(httptrace.WroteRequestInfo{Err: tc.writeError})
						}
						if tc.status == 0 {
							if tc.writeError != nil {
								return nil, tc.writeError
							}
							return nil, context.DeadlineExceeded
						}
						return &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				})}

				result, provider, err := manager.SearchWithBestProvider(t.Context(), SearchRequest{Query: "fixture"})

				if tc.send {
					require.Equal(t, 1, calls, "a possibly sent search must never reach the backup provider")
					require.Nil(t, result)
					require.ErrorIs(t, err, ErrSearchOutcomeUnknown)
				} else {
					require.NoError(t, err)
					require.Equal(t, 2, calls)
					require.Equal(t, backup, provider)
				}
			})
		}
	}
}

func TestSearchTransportDoesNotReplayWrittenGET(t *testing.T) {
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if received.Add(1) == 2 {
			// The request reached the provider, but the connection fails
			// before it returns the response headers.
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	client, err := newHTTPClient("")
	require.NoError(t, err)
	defer client.CloseIdleConnections()
	search := func() error {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
		require.NoError(t, err)
		var response map[string]any
		return searchJSON(client, request, "fixture", &response)
	}
	require.NoError(t, search(), "warm a keep-alive connection if permitted")

	err = search()

	require.Equal(t, int32(2), received.Load(), "the HTTP transport must not reissue a search already received by the server")
	require.ErrorIs(t, err, ErrSearchOutcomeUnknown)
}

func TestManager_SearchWithBestProvider_NilRedis(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := braveResponse{}
		resp.Web.Results = []braveResult{{URL: "https://test.com", Title: "Test", Description: "result"}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	origURL := *braveSearchURL
	u, _ := http.NewRequest("GET", srv.URL, nil)
	*braveSearchURL = *u.URL
	defer func() { *braveSearchURL = origURL }()

	m := NewManager([]ProviderConfig{
		{Type: "brave", APIKey: "k", QuotaLimit: 100},
	}, nil)
	m.clientCache[""] = srv.Client()

	resp, _, err := m.SearchWithBestProvider(context.Background(), SearchRequest{Query: "test"})
	require.NoError(t, err)
	require.Len(t, resp.Results, 1)
}

func TestManager_GetUsage_NilRedis(t *testing.T) {
	m := NewManager(nil, nil)
	used, err := m.GetUsage(context.Background(), "brave")
	require.NoError(t, err)
	require.Equal(t, int64(0), used)
}

func TestManager_GetAllUsage_NilRedis(t *testing.T) {
	m := NewManager([]ProviderConfig{
		{Type: "brave"},
	}, nil)
	usage := m.GetAllUsage(context.Background())
	require.Equal(t, int64(0), usage["brave"])
}

// --- Quota TTL from subscription ---

func TestQuotaTTLFromSubscription_NilSubscription(t *testing.T) {
	ttl := quotaTTLFromSubscription(nil)
	require.Equal(t, defaultQuotaTTL, ttl)
}

func TestQuotaTTLFromSubscription_ZeroSubscription(t *testing.T) {
	zero := int64(0)
	ttl := quotaTTLFromSubscription(&zero)
	require.Equal(t, defaultQuotaTTL, ttl)
}

func TestQuotaTTLFromSubscription_ValidSubscription(t *testing.T) {
	// Subscribed 10 days ago — next reset in ~20 days
	sub := time.Now().Add(-10 * 24 * time.Hour).Unix()
	ttl := quotaTTLFromSubscription(&sub)
	require.Greater(t, ttl, 15*24*time.Hour) // at least 15 days
	require.Less(t, ttl, 25*24*time.Hour+quotaTTLBuffer)
}

func TestNextMonthlyReset_SubscribedRecentPast(t *testing.T) {
	// Subscribed on the 10th of this month (always valid day)
	now := time.Now().UTC()
	sub := time.Date(now.Year(), now.Month(), 10, 0, 0, 0, 0, time.UTC)
	next := nextMonthlyReset(sub)
	require.True(t, next.After(now) || next.Equal(now), "next reset should be in the future or now")
	require.True(t, next.Before(now.AddDate(0, 1, 1)))
}

func TestNextMonthlyReset_SubscribedLongAgo(t *testing.T) {
	// Subscribed 6 months ago on the 1st
	sub := time.Now().UTC().AddDate(0, -6, 0)
	sub = time.Date(sub.Year(), sub.Month(), 1, 0, 0, 0, 0, time.UTC)
	next := nextMonthlyReset(sub)
	require.True(t, next.After(time.Now().UTC()))
	// Should be within the next 31 days
	require.True(t, next.Before(time.Now().UTC().AddDate(0, 1, 1)))
}

func TestNextMonthlyReset_FutureSubscription(t *testing.T) {
	sub := time.Now().UTC().AddDate(0, 0, 5)
	next := nextMonthlyReset(sub)
	require.True(t, next.After(time.Now().UTC()))
}

func TestAddMonthsClamped_Jan31ToFeb(t *testing.T) {
	sub := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	next := addMonthsClamped(sub, 1)
	require.Equal(t, time.Month(2), next.Month())
	require.Equal(t, 28, next.Day()) // Feb 28 (2026 is not a leap year)
}

func TestAddMonthsClamped_Jan31ToFebLeapYear(t *testing.T) {
	sub := time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC)
	next := addMonthsClamped(sub, 1)
	require.Equal(t, time.Month(2), next.Month())
	require.Equal(t, 29, next.Day()) // Feb 29 (2028 is a leap year)
}

func TestAddMonthsClamped_Mar31ToApr(t *testing.T) {
	sub := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	next := addMonthsClamped(sub, 1)
	require.Equal(t, time.Month(4), next.Month())
	require.Equal(t, 30, next.Day()) // Apr has 30 days
}

func TestAddMonthsClamped_NormalDay(t *testing.T) {
	sub := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	next := addMonthsClamped(sub, 1)
	require.Equal(t, time.Month(2), next.Month())
	require.Equal(t, 15, next.Day()) // no clamping needed
}

// --- Redis key ---

func TestQuotaRedisKey_Format(t *testing.T) {
	key := quotaRedisKey("brave")
	require.Equal(t, "websearch:quota:brave", key)
}

// --- isProviderAvailable ---

func TestIsProviderAvailable_EmptyAPIKey(t *testing.T) {
	m := NewManager(nil, nil)
	require.False(t, m.isProviderAvailable(ProviderConfig{APIKey: ""}))
}

func TestIsProviderAvailable_Expired(t *testing.T) {
	m := NewManager(nil, nil)
	past := time.Now().Add(-1 * time.Hour).Unix()
	require.False(t, m.isProviderAvailable(ProviderConfig{APIKey: "k", ExpiresAt: &past}))
}

func TestIsProviderAvailable_Valid(t *testing.T) {
	m := NewManager(nil, nil)
	future := time.Now().Add(1 * time.Hour).Unix()
	require.True(t, m.isProviderAvailable(ProviderConfig{APIKey: "k", ExpiresAt: &future}))
	require.True(t, m.isProviderAvailable(ProviderConfig{APIKey: "k"})) // no expiry
}

// --- resolveProxyID ---

func TestResolveProxyID_AccountProxyOverrides(t *testing.T) {
	cfg := ProviderConfig{ProxyID: 42}
	require.Equal(t, int64(0), resolveProxyID(cfg, "http://account-proxy:8080"))
	require.Equal(t, int64(42), resolveProxyID(cfg, ""))
}

// --- isProxyError ---

func TestIsProxyError_Nil(t *testing.T) {
	require.False(t, isProxyError(nil))
}

func TestIsProxyError_ConnectionRefused(t *testing.T) {
	require.True(t, isProxyError(fmt.Errorf("dial tcp: connection refused")))
}

func TestIsProxyError_Timeout(t *testing.T) {
	require.True(t, isProxyError(fmt.Errorf("i/o timeout while connecting to proxy")))
}

func TestIsProxyError_SOCKS(t *testing.T) {
	require.True(t, isProxyError(fmt.Errorf("socks connect failed")))
}

func TestIsProxyError_TLSHandshake(t *testing.T) {
	require.True(t, isProxyError(fmt.Errorf("tls handshake timeout")))
}

func TestIsProxyError_APIError_NotProxy(t *testing.T) {
	require.False(t, isProxyError(fmt.Errorf("API rate limit exceeded")))
}

// --- isProxyAvailable (nil Redis) ---

func TestIsProxyAvailable_NilRedis(t *testing.T) {
	m := NewManager(nil, nil)
	require.True(t, m.isProxyAvailable(context.Background(), 42))
}

func TestIsProxyAvailable_ZeroID(t *testing.T) {
	m := NewManager(nil, nil)
	require.True(t, m.isProxyAvailable(context.Background(), 0))
}

// --- selectByQuotaWeight ---

func TestSelectByQuotaWeight_NoQuotaLast(t *testing.T) {
	m := NewManager(nil, nil)
	candidates := []ProviderConfig{
		{Type: "brave", APIKey: "k1", QuotaLimit: 0},
		{Type: "tavily", APIKey: "k2", QuotaLimit: 100},
	}
	result := m.selectByQuotaWeight(context.Background(), candidates)
	require.Len(t, result, 2)
	require.Equal(t, "tavily", result[0].Type)
	require.Equal(t, "brave", result[1].Type)
}

func TestSelectByQuotaWeight_AllNoQuota(t *testing.T) {
	m := NewManager(nil, nil)
	candidates := []ProviderConfig{
		{Type: "brave", APIKey: "k1", QuotaLimit: 0},
		{Type: "tavily", APIKey: "k2", QuotaLimit: 0},
	}
	result := m.selectByQuotaWeight(context.Background(), candidates)
	require.Len(t, result, 2)
}

func TestSelectByQuotaWeight_Empty(t *testing.T) {
	m := NewManager(nil, nil)
	result := m.selectByQuotaWeight(context.Background(), nil)
	require.Empty(t, result)
}

// --- newHTTPClient ---

func TestNewHTTPClient_NoProxy(t *testing.T) {
	c, err := newHTTPClient("")
	require.NoError(t, err)
	require.NotNil(t, c)
}

func TestNewHTTPClient_InvalidProxy(t *testing.T) {
	_, err := newHTTPClient("://bad-url")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid proxy URL")
}

func TestNewHTTPClient_ValidHTTPProxy(t *testing.T) {
	c, err := newHTTPClient("http://proxy.example.com:8080")
	require.NoError(t, err)
	require.NotNil(t, c)
}

func TestNewHTTPClient_ValidSOCKS5Proxy(t *testing.T) {
	c, err := newHTTPClient("socks5://proxy.example.com:1080")
	require.NoError(t, err)
	require.NotNil(t, c)
}

// --- ResetUsage ---

func TestManager_ResetUsage_NilRedis(t *testing.T) {
	m := NewManager(nil, nil)
	err := m.ResetUsage(context.Background(), "brave")
	require.NoError(t, err)
}
