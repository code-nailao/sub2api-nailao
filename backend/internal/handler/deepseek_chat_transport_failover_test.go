//go:build unit

package handler

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type deepSeekChatFailoverUpstream struct {
	service.HTTPUpstream
	first    func(*http.Request) (*http.Response, error)
	attempts []int64
}

func (u *deepSeekChatFailoverUpstream) Do(req *http.Request, proxyURL string, accountID int64, concurrency int) (*http.Response, error) {
	u.attempts = append(u.attempts, accountID)
	if u.first == nil {
		return u.HTTPUpstream.Do(req, proxyURL, accountID, concurrency)
	}
	if len(u.attempts) == 1 {
		return u.first(req)
	}
	_ = req.Body.Close()
	return deepSeekChatFailoverResponse(req.Header.Get("Accept") == "text/event-stream"), nil
}

func deepSeekChatFailoverResponse(stream bool) *http.Response {
	body := `{"id":"fixture","object":"chat.completion","model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant","content":"fixture"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`
	header := http.Header{"Content-Type": {"application/json"}}
	if stream {
		body = "data: " + `{"id":"fixture","object":"chat.completion.chunk","model":"deepseek-flash","choices":[{"index":0,"delta":{"content":"fixture"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}` + "\n\ndata: [DONE]\n\n"
		header.Set("Content-Type", "text/event-stream")
	}
	return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func TestDeepSeekChatTransportFailover(t *testing.T) {
	// The advanced scheduler setting is cached process-wide.
	const child = "SUB2API_TEST_DEEPSEEK_CHAT_FAILOVER_CHILD"
	if os.Getenv(child) != "1" {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		binary, err := os.Executable()
		require.NoError(t, err)
		cmd := exec.CommandContext(ctx, binary, "-test.run=^TestDeepSeekChatTransportFailover$")
		cmd.Env = append(os.Environ(), child+"=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		return
	}

	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t/full_client_cache", stream), func(t *testing.T) {
			testDeepSeekChatFailoverWithFullClientCache(t, stream)
		})
		for _, failure := range []string{"disconnect", "dial_timeout", "dns_validation"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, failure), func(t *testing.T) {
				received := make(chan []byte, 1)
				transport := &http.Transport{DialContext: func(_ context.Context, network, _ string) (net.Conn, error) {
					if failure == "dial_timeout" {
						return nil, &net.OpError{Op: "dial", Net: network, Err: context.DeadlineExceeded}
					}
					client, server := net.Pipe()
					go func() {
						defer func() { _ = server.Close() }()
						_ = server.SetDeadline(time.Now().Add(2 * time.Second))
						req, err := http.ReadRequest(bufio.NewReader(server))
						if err != nil {
							return
						}
						data, err := io.ReadAll(req.Body)
						if err == nil {
							received <- data
						}
						// Close after reading the entire body, without writing a status.
					}()
					return client, nil
				}}
				t.Cleanup(transport.CloseIdleConnections)
				client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
				upstream := &deepSeekChatFailoverUpstream{first: func(req *http.Request) (*http.Response, error) {
					if failure == "dns_validation" {
						_ = req.Body.Close()
						return nil, fmt.Errorf("dns resolution failed: %w", &net.DNSError{IsTimeout: true})
					}
					// Run the standard HTTP transport through an in-memory pipe.
					req = req.Clone(req.Context())
					req.URL.Scheme = "http"
					return client.Do(req)
				}}
				h, key, usage, billing := newDeepSeekChatFailoverHandler(t, upstream, "")
				body := fmt.Sprintf(`{"model":"deepseek-flash","messages":[{"role":"user","content":"fixture"}],"stream":%t}`, stream)
				rec := serveRejectionHTTPBody(t, h, key, "chat/completions", body)
				metrics := h.gatewayService.SnapshotOpenAIAccountSchedulerMetrics()
				require.Positive(t, metrics.SelectTotal, "the test must exercise advanced scheduling")
				if failure != "disconnect" {
					require.Empty(t, received)
					require.Len(t, upstream.attempts, 2)
					require.NotEqual(t, upstream.attempts[0], upstream.attempts[1])
					require.EqualValues(t, 1, metrics.AccountSwitchTotal)
					require.Equal(t, 2, metrics.RuntimeStatsAccountCount)
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					require.Len(t, usage.created, 1)
					require.Len(t, billing.commands, 1)
					require.Equal(t, upstream.attempts[1], billing.commands[0].AccountID)
					require.Positive(t, billing.commands[0].BalanceCost)
					return
				}
				select {
				case forwarded := <-received:
					require.True(t, gjson.ValidBytes(forwarded))
					require.Equal(t, stream, gjson.GetBytes(forwarded, "stream").Bool())
				default:
					t.Fatal("the simulated upstream did not read the complete request")
				}
				require.Len(t, upstream.attempts, 1, "an unknown outcome must not trigger a second send")
				require.Zero(t, metrics.AccountSwitchTotal)
				require.Equal(t, 1, metrics.RuntimeStatsAccountCount, "report the failed attempt for the selected account")
				require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
				require.Equal(t, "upstream_error", gjson.Get(rec.Body.String(), "error.type").String())
				require.Empty(t, usage.created)
				require.Empty(t, billing.commands)
			})
		}
	}
}

const (
	deepSeekChatPreferredAccountID = 1
	deepSeekChatBackupAccountID    = 2
	deepSeekChatAccountConcurrency = 2
)

func testDeepSeekChatFailoverWithFullClientCache(t *testing.T, stream bool) {
	t.Helper()
	var modelCalls atomic.Int64
	releaseHold := make(chan struct{})
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			w.WriteHeader(http.StatusOK)
			_ = http.NewResponseController(w).Flush()
			<-releaseHold
			return
		}
		modelCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		response := deepSeekChatFailoverResponse(stream)
		defer func() { _ = response.Body.Close() }()
		w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(upstreamServer.Close)
	t.Cleanup(func() { close(releaseHold) })
	realUpstream := repository.NewHTTPUpstream(&config.Config{Gateway: config.GatewayConfig{
		ConnectionPoolIsolation: config.ConnectionPoolIsolationAccount,
		MaxUpstreamClients:      1,
	}})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	hold, err := http.NewRequestWithContext(service.WithHTTPUpstreamProfile(ctx, service.HTTPUpstreamProfileOpenAI), http.MethodGet, upstreamServer.URL+"/hold", nil)
	require.NoError(t, err)
	inFlight, err := realUpstream.Do(hold, "", deepSeekChatBackupAccountID, deepSeekChatAccountConcurrency)
	require.NoError(t, err)
	defer func() { _ = inFlight.Body.Close() }()

	upstream := &deepSeekChatFailoverUpstream{HTTPUpstream: realUpstream}
	h, key, usage, billing := newDeepSeekChatFailoverHandler(t, upstream, upstreamServer.URL)
	body := fmt.Sprintf(`{"model":"deepseek-flash","messages":[{"role":"user","content":"fixture"}],"stream":%t}`, stream)
	rec := serveRejectionHTTPBody(t, h, key, "chat/completions", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []int64{deepSeekChatPreferredAccountID, deepSeekChatBackupAccountID}, upstream.attempts)
	require.EqualValues(t, 1, modelCalls.Load(), "only the account with a cached client can send the request")
	require.EqualValues(t, 1, h.gatewayService.SnapshotOpenAIAccountSchedulerMetrics().AccountSwitchTotal)
	require.Len(t, usage.created, 1)
	require.Len(t, billing.commands, 1)
	require.EqualValues(t, deepSeekChatBackupAccountID, billing.commands[0].AccountID)
}

type deepSeekChatFailoverBalanceCache struct{ *handlerInflightCache }

func (c deepSeekChatFailoverBalanceCache) DeductUserBalance(_ context.Context, _ int64, cost float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.balance -= cost
	return nil
}

func newDeepSeekChatFailoverHandler(t *testing.T, upstream service.HTTPUpstream, baseURL string) (*OpenAIGatewayHandler, *service.APIKey, *openAIWSUsageHandlerUsageLogRepoStub, *compatStreamUsageBillingRepo) {
	t.Helper()
	cfg := &config.Config{RunMode: config.RunModeStandard}
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.MaxAccountSwitches = 1
	cfg.Gateway.OpenAIWS.LBTopK = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 1
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	group := &service.Group{
		ID: 1, Platform: service.PlatformDeepseek, Status: service.StatusActive, Hydrated: true,
		SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 1,
	}
	accounts := make([]service.Account, 2)
	for i := range accounts {
		accounts[i] = service.Account{
			ID: int64(deepSeekChatPreferredAccountID + i), Platform: service.PlatformDeepseek, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{group.ID}, Concurrency: deepSeekChatAccountConcurrency,
			Priority:    i,
			Credentials: map[string]any{"api_key": "fixture-only", "api_protocol": "chat_completions"},
		}
		if baseURL != "" {
			accounts[i].Credentials["base_url"] = baseURL
		}
	}
	repo := &openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}
	usage := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, len(accounts))}
	ledger := &compatStreamUsageBillingRepo{}
	billing := service.NewBillingCacheService(deepSeekChatFailoverBalanceCache{newHandlerInflightCache(1)}, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	slots := service.NewConcurrencyService(&fakeConcurrencyCache{})
	rate := service.NewRateLimitService(repo, nil, cfg, nil, nil)
	rate.SetSettingService(service.NewSettingService(chatToolFilterSchedulerSettings{}, cfg))
	gateway := service.NewOpenAIGatewayService(repo, usage, ledger, nil, nil, nil, nil, cfg, nil, slots,
		service.NewBillingService(cfg, nil), rate, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	h := NewOpenAIGatewayHandler(gateway, slots, billing, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
	user := &service.User{ID: 1, Status: service.StatusActive}
	key := &service.APIKey{ID: 1, UserID: user.ID, User: user, GroupID: &group.ID, Group: group}
	return h, key, usage, ledger
}
