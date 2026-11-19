package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The values are deliberately fake legacy values.
func setMediaEngineLegacy(t *testing.T, name, prefix string) {
	t.Helper()
	resetViperWithJWTSecret(t)
	t.Setenv(SourcesMediaEngineLegacyNameEnv, name)
	t.Setenv(SourcesMediaEngineLegacyTaskPrefixEnv, prefix)
}

func TestLoadMediaEngineLegacySettings(t *testing.T) {
	t.Run("unset selects the neutral namespace", func(t *testing.T) {
		setMediaEngineLegacy(t, "", "")
		cfg, err := Load()
		require.NoError(t, err)
		require.Empty(t, cfg.Gateway.MediaEngineLegacyName)
		require.Empty(t, cfg.Gateway.MediaEngineLegacyTaskPrefix)
	})

	t.Run("plain values are read from the environment", func(t *testing.T) {
		setMediaEngineLegacy(t, "legacyname", "legacytask")
		cfg, err := Load()
		require.NoError(t, err)
		require.Equal(t, "legacyname", cfg.Gateway.MediaEngineLegacyName)
		require.Equal(t, "legacytask", cfg.Gateway.MediaEngineLegacyTaskPrefix)
	})

	t.Run("a config file cannot set them", func(t *testing.T) {
		setMediaEngineLegacy(t, "", "")
		for _, name := range []string{"GATEWAY_MEDIAENGINELEGACYNAME", "GATEWAY_MEDIA_ENGINE_LEGACY_NAME", "GATEWAY_MEDIAENGINELEGACYTASKPREFIX", "GATEWAY_MEDIA_ENGINE_LEGACY_TASK_PREFIX"} {
			t.Setenv(name, "legacyvalue")
		}
		cfg, err := Load()
		require.NoError(t, err)
		require.Empty(t, cfg.Gateway.MediaEngineLegacyName)
		require.Empty(t, cfg.Gateway.MediaEngineLegacyTaskPrefix)
	})

	for _, invalid := range []string{"Legacyname", "legacy-name", "legacy_name", " legacyname", "legacyname\n", "x", "legacy.name", "sub2api", strings.Repeat("a", 33)} {
		t.Run("rejects name "+strings.TrimSpace(invalid[:min(len(invalid), 16)]), func(t *testing.T) {
			setMediaEngineLegacy(t, invalid, "legacytask")
			_, err := Load()
			require.ErrorContains(t, err, SourcesMediaEngineLegacyNameEnv)
			require.NotContains(t, err.Error(), strings.TrimSpace(invalid), "the private value is not echoed")
		})
	}
	for _, invalid := range []string{"Legacytask", "legacy-task", "legacy_task", " legacytask", "q", strings.Repeat("b", 17)} {
		t.Run("rejects task prefix "+strings.TrimSpace(invalid[:min(len(invalid), 16)]), func(t *testing.T) {
			setMediaEngineLegacy(t, "legacyname", invalid)
			_, err := Load()
			require.ErrorContains(t, err, SourcesMediaEngineLegacyTaskPrefixEnv)
			require.NotContains(t, err.Error(), strings.TrimSpace(invalid), "the private value is not echoed")
		})
	}
	for _, tc := range []struct{ name, prefix string }{{"legacyname", ""}, {"", "legacytask"}} {
		t.Run("rejects only one of the two", func(t *testing.T) {
			setMediaEngineLegacy(t, tc.name, tc.prefix)
			_, err := Load()
			require.ErrorContains(t, err, SourcesMediaEngineLegacyNameEnv)
			require.ErrorContains(t, err, SourcesMediaEngineLegacyTaskPrefixEnv)
			require.ErrorContains(t, err, "set together")
			require.NotContains(t, err.Error(), "legacyname")
			require.NotContains(t, err.Error(), "legacytask")
		})
	}
}
