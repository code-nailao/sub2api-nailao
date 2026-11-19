package repository

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestWalletTransportDoesNotReuseOrReconfigureModelPool(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIResponseHeaderTimeout = 20
	upstream, ok := NewHTTPUpstream(cfg).(*httpUpstreamService)
	require.True(t, ok)
	model, err := upstream.getClientEntry("", 7, 1, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(t, err)
	wallet, err := upstream.getClientEntry("", 7, 1, service.HTTPUpstreamProfileWallet, false, false)
	require.NoError(t, err)
	require.NotSame(t, model, wallet)
	again, err := upstream.getClientEntry("", 7, 1, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(t, err)
	require.Same(t, model, again, "wallet requests must not replace the model pool")
	settings := upstream.applyProfilePoolSettings(defaultPoolSettings(cfg), service.HTTPUpstreamProfileWallet)
	require.Equal(t, 360*time.Second, settings.responseHeaderTimeout)
	settings = upstream.applyProfilePoolSettings(defaultPoolSettings(cfg), service.HTTPUpstreamProfileOpenAI)
	require.Equal(t, 20*time.Second, settings.responseHeaderTimeout)
}
