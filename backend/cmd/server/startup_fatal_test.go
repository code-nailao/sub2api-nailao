package main

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
)

func TestFatalStartupErrorSurvivesWarnLogging(t *testing.T) {
	const childFlag = "SOURCES_STARTUP_FATAL_TEST"
	const message = "Failed to initialize application: atomic native image ledger is required"
	if os.Getenv(childFlag) == "1" {
		err := logger.Init(logger.InitOptions{
			Level:  "warn",
			Output: logger.OutputOptions{ToStdout: true},
		})
		require.NoError(t, err)
		// This was the old Fatalf output path: the bridge classifies it as INFO.
		log.Print(message)
		fatalStartupError("initialize application", errors.New("atomic native image ledger is required"))
		return
	}

	// #nosec G702 -- Re-executes this test binary with a fixed argument.
	cmd := exec.Command(os.Args[0], "-test.run=^TestFatalStartupErrorSurvivesWarnLogging$")
	cmd.Env = append(os.Environ(), childFlag+"=1")
	stdout, err := cmd.Output()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	require.Equal(t, 1, exit.ExitCode())
	require.Empty(t, stdout)
	require.Equal(t, message+"\n", string(exit.Stderr))
}
