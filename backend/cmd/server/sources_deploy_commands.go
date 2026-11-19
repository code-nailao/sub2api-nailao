package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/processrole"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/setup"
	"github.com/Wei-Shaw/sub2api/internal/web"
	"github.com/redis/go-redis/v9"
)

// One-shot commands used by the release script. Both exit without serving
// HTTP, taking the background ownership lock or starting background services.
// Application logs share stdout, so each command reports exactly one result
// line that starts with its own prefix; scripts read only that line.
const (
	sourcesPendingMigrationsPrefix = "SOURCES_PENDING_MIGRATIONS"
	sourcesStartupCheckPrefix      = "SOURCES_STARTUP_CHECK_OK"
	sourcesWebAssetsPrefix         = "SOURCES_WEB_ASSETS_EXPORTED"
)

var (
	sourcesListPendingMigrations = flag.Bool("list-pending-migrations", false,
		"Print one line '"+sourcesPendingMigrationsPrefix+" a.sql,b.sql' (empty list when current) for -migrate-only, then exit")
	sourcesCheckStartup = flag.Bool("check-startup", false,
		"Construct every dependency with serving semantics against the configured database, then exit without serving or starting background services")
	sourcesExportWebAssets = flag.String("export-web-assets", "",
		"Copy the embedded frontend assets into this absolute directory (additive, never overwrites), print one '"+sourcesWebAssetsPrefix+" <count>' line, then exit")
)

// runSourcesDeployCommand runs a requested one-shot command and reports whether
// one ran; the caller must then return from main. Failures exit non-zero.
func runSourcesDeployCommand(conflicting bool) bool {
	requested := 0
	for _, on := range []bool{*sourcesListPendingMigrations, *sourcesCheckStartup, *sourcesExportWebAssets != ""} {
		if on {
			requested++
		}
	}
	if requested == 0 {
		return false
	}
	if conflicting || requested > 1 {
		fmt.Fprintln(os.Stderr, "Deployment commands run alone; do not combine them with each other, setup, version or migrate-only")
		os.Exit(2)
	}
	if *sourcesExportWebAssets != "" {
		written, err := web.ExportAssets(*sourcesExportWebAssets)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Export web assets failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%s %d\n", sourcesWebAssetsPrefix, written)
		return true
	}
	if *sourcesListPendingMigrations {
		if err := runSourcesListPendingMigrations(os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "List pending migrations failed: %v\n", err)
			os.Exit(1)
		}
		return true
	}
	if err := runSourcesStartupCheck(os.Stdout); err != nil {
		// Same direct stderr path as fatalStartupError: log settings must not
		// hide why a candidate cannot start.
		fmt.Fprintf(os.Stderr, "Startup check failed: %v\n", err)
		os.Exit(1)
	}
	return true
}

func openSourcesDeploymentDB() (*sql.DB, error) {
	cfg, err := config.LoadForBootstrap()
	if err != nil {
		return nil, err
	}
	// Driver startup parameters survive a connection replacement and bound
	// lock waits and statements; the caller bounds the whole operation.
	dsn := cfg.Database.DSNWithTimezone(cfg.Timezone) + " connect_timeout=5 lock_timeout=2000 statement_timeout=30000"
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

// runSourcesListPendingMigrations prints the result line; the list after the
// prefix is empty when the database is current. It only reads the ledger in a
// read-only transaction.
func runSourcesListPendingMigrations(w io.Writer) error {
	db, err := openSourcesDeploymentDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pending, err := repository.ListSourcesPendingMigrations(ctx, db)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s %s\n", sourcesPendingMigrationsPrefix, strings.Join(pending, ","))
	return err
}

// sourcesStartupCheckReport is the JSON that follows the success prefix.
type sourcesStartupCheckReport struct {
	Version            string   `json:"version"`
	Commit             string   `json:"commit"`
	BackgroundServices []string `json:"background_services"`
}

// runSourcesStartupCheck builds the full dependency graph exactly as a serving
// candidate does: migrations are verified read-only (so pending ones must be
// applied first), secrets are read not created, media engine opens as a producer, no
// seed or settings migration runs and every background service is registered
// but not started. It catches wiring failures such as a missing repository
// capability before any traffic or ownership moves. Branches that run only in
// the lock-owning background (applying migrations, opening the media engine)
// are not exercised here.
func runSourcesStartupCheck(w io.Writer) error {
	cleanup, err := prepareSourcesStartupCheckEnv()
	if err != nil {
		return err
	}
	defer cleanup()
	runtime, err := processrole.Initialize()
	if err != nil {
		return fmt.Errorf("initialize serving-semantics runtime: %w", err)
	}
	if setup.NeedsSetup() {
		return errors.New("database and deployment configuration are not initialized")
	}
	cfg, err := config.LoadForBootstrap()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := logger.Init(logger.OptionsFromConfig(cfg.Log)); err != nil {
		return fmt.Errorf("initialize logger: %w", err)
	}
	// Reachability first: constructors connect with retries, so an unreachable
	// Redis or database would otherwise stall the graph for minutes before the
	// check below could report it. A background owner that cannot reach Redis
	// holds the ownership lock while it retries, which blocks the handoff for
	// the whole readiness timeout; fail here instead, in seconds.
	if err := checkSourcesDependencies(cfg); err != nil {
		return err
	}
	app, err := initializeApplication(handler.BuildInfo{Version: Version, BuildType: BuildType})
	if err != nil {
		return fmt.Errorf("initialize application: %w", err)
	}
	app.Cleanup()

	services := make([]string, 0)
	for name := range runtime.Status().BackgroundServices {
		services = append(services, name)
	}
	sort.Strings(services)
	report, err := json.Marshal(sourcesStartupCheckReport{Version: Version, Commit: Commit, BackgroundServices: services})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s %s\n", sourcesStartupCheckPrefix, report)
	return err
}

// checkSourcesDependencies proves the deployment can reach Redis and the
// database with the configured credentials, using short timeouts and no writes.
func checkSourcesDependencies(cfg *config.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	// A dedicated client without retries: the application client retries with
	// backoff, which turns an unreachable host into minutes instead of seconds.
	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Redis.Address(),
		Username:     cfg.Redis.Username,
		Password:     cfg.Redis.Password,
		DB:           cfg.Redis.DB,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolTimeout:  3 * time.Second,
		MaxRetries:   -1, // one attempt
	})
	defer func() { _ = client.Close() }()
	if err := client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis %s is not reachable: %w", cfg.Redis.Address(), err)
	}
	db, err := openSourcesDeploymentDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("database is not reachable: %w", err)
	}
	return nil
}

// prepareSourcesStartupCheckEnv forces serving semantics for this process with
// a private, throwaway lock directory and runtime token, so the check never
// competes for the real background lock. It also drops the media engine admission
// journal path: opening it takes the live serving instance's journal lock.
func prepareSourcesStartupCheckEnv() (func(), error) {
	dir, err := os.MkdirTemp("", "sources-startup-check-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := os.Chmod(dir, 0o700); err != nil {
		cleanup()
		return nil, err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		cleanup()
		return nil, err
	}
	tokenFile := filepath.Join(dir, "runtime.token")
	if err := os.WriteFile(tokenFile, []byte(hex.EncodeToString(raw[:])+"\n"), 0o600); err != nil {
		cleanup()
		return nil, err
	}
	for name, value := range map[string]string{
		"SOURCES_PROCESS_ROLE":        string(processrole.Serving),
		"SOURCES_BACKGROUND_LOCK_DIR": dir,
		"SOURCES_RUNTIME_TOKEN_FILE":  tokenFile,
	} {
		if err := os.Setenv(name, value); err != nil {
			cleanup()
			return nil, err
		}
	}
	if err := os.Unsetenv("SOURCES_MEDIA_ENGINE_ADMISSION_STATE_FILE"); err != nil {
		cleanup()
		return nil, err
	}
	return cleanup, nil
}
