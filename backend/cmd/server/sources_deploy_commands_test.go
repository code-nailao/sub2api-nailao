package main

import (
	"os"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/processrole"
	"github.com/stretchr/testify/require"
)

func TestSourcesStartupCheckEnvForcesPrivateServingRuntime(t *testing.T) {
	for _, name := range []string{"SOURCES_PROCESS_ROLE", "SOURCES_BACKGROUND_LOCK_DIR", "SOURCES_RUNTIME_TOKEN_FILE"} {
		t.Setenv(name, "")
	}
	t.Setenv("SOURCES_PROCESS_ROLE", string(processrole.Background))
	t.Setenv("SOURCES_BACKGROUND_LOCK_DIR", "/live/background-lock")
	t.Setenv("SOURCES_MEDIA_ENGINE_ADMISSION_STATE_FILE", "/live/admission/state.json")

	cleanup, err := prepareSourcesStartupCheckEnv()
	require.NoError(t, err)
	dir := os.Getenv("SOURCES_BACKGROUND_LOCK_DIR")

	cfg, err := processrole.ConfigFromEnv()
	require.NoError(t, err)
	require.Equal(t, processrole.Serving, cfg.Role, "the check must never run with background semantics")
	require.NotEqual(t, "/live/background-lock", cfg.LockDir, "the check must not touch the real ownership lock directory")
	require.Len(t, cfg.Token, 64)
	_, present := os.LookupEnv("SOURCES_MEDIA_ENGINE_ADMISSION_STATE_FILE")
	require.False(t, present, "the live admission journal lock must not be opened")

	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	cleanup()
	_, err = os.Stat(dir)
	require.True(t, os.IsNotExist(err), "the throwaway lock directory is removed")
}

func TestSourcesDeployCommandIsNoOpWithoutFlags(t *testing.T) {
	require.False(t, *sourcesListPendingMigrations)
	require.False(t, *sourcesCheckStartup)
	require.Empty(t, *sourcesExportWebAssets)
	require.False(t, runSourcesDeployCommand(true), "no command requested means normal startup continues")
}
