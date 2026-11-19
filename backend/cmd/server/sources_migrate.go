package main

import (
	"context"
	"errors"
	"flag"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/repository"
)

func sourcesMigrationRequested(flags *flag.FlagSet) bool {
	requested := false
	flags.Visit(func(value *flag.Flag) {
		if value.Name == "migrate-only" {
			requested = true
		}
	})
	return requested
}

// Only additive changes reviewed against the running version belong here. This
// path runs before setup, process-role initialization or Wire construction: it
// cannot start runners, seed users, change Redis slots or create a signing key.
func runSourcesMigrationsOnly(approved string) error {
	if strings.TrimSpace(approved) == "" {
		return errors.New("migration-only requires the exact approved pending filenames")
	}
	// The two-second lock_timeout bounds SQL lock waits; the native
	// advisory-lock loop is separately bounded by the overall context below.
	// All runner work, including its native advisory lock, uses one connection.
	db, err := openSourcesDeploymentDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := repository.CheckSourcesDeploymentMigrationPlan(ctx, db, strings.Split(approved, ",")); err != nil {
		return err
	}
	return repository.ApplyMigrations(ctx, db)
}
