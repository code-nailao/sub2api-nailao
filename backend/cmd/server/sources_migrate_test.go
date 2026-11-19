package main

import (
	"flag"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourcesEmptyMigrationApprovalNeverSelectsFullMain(t *testing.T) {
	for _, args := range [][]string{{"-migrate-only="}, {"-migrate-only", ""}, {"-migrate-only", "  "}} {
		flags := flag.NewFlagSet("migration-fixture", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		approved := flags.String("migrate-only", "", "")
		require.NoError(t, flags.Parse(args))
		require.True(t, sourcesMigrationRequested(flags))
		require.ErrorContains(t, runSourcesMigrationsOnly(*approved), "exact approved pending filenames")
	}
	flags := flag.NewFlagSet("normal-fixture", flag.ContinueOnError)
	flags.String("migrate-only", "", "")
	require.NoError(t, flags.Parse(nil))
	require.False(t, sourcesMigrationRequested(flags))
}
