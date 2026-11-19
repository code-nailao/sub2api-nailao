package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadUpstreamRejectionLegacyProfile(t *testing.T) {
	t.Run("unset leaves the canonical id in charge", func(t *testing.T) {
		resetViperWithJWTSecret(t)
		t.Setenv(SourcesUpstreamRejectionLegacyProfileEnv, "")
		cfg, err := Load()
		require.NoError(t, err)
		require.Empty(t, cfg.Gateway.UpstreamRejectionLegacyProfile)
	})

	t.Run("a plain identifier is read from the environment", func(t *testing.T) {
		resetViperWithJWTSecret(t)
		t.Setenv(SourcesUpstreamRejectionLegacyProfileEnv, "legacy_profile_v0")
		cfg, err := Load()
		require.NoError(t, err)
		require.Equal(t, "legacy_profile_v0", cfg.Gateway.UpstreamRejectionLegacyProfile)
	})

	for _, invalid := range []string{"Legacy_Profile", "legacy-profile", " legacy_profile_v0", "legacy_profile_v0\n", "legacy.profile", strings.Repeat("a", 65)} {
		t.Run("rejects "+strings.TrimSpace(invalid[:min(len(invalid), 16)]), func(t *testing.T) {
			resetViperWithJWTSecret(t)
			t.Setenv(SourcesUpstreamRejectionLegacyProfileEnv, invalid)
			_, err := Load()
			require.ErrorContains(t, err, SourcesUpstreamRejectionLegacyProfileEnv)
			require.NotContains(t, err.Error(), invalid, "the private value is not echoed")
		})
	}
}
