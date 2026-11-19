package main

//go:generate go run github.com/google/wire/cmd/wire

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/processrole"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/setup"
	"github.com/Wei-Shaw/sub2api/internal/web"

	"github.com/gin-gonic/gin"
)

//go:embed VERSION
var embeddedVersion string

// Build-time variables (can be set by ldflags)
var (
	Version   = ""
	Commit    = "unknown"
	Date      = "unknown"
	BuildType = "source" // "source" for manual builds, "release" for CI builds (set by ldflags)
)

func init() {
	// 如果 Version 已通过 ldflags 注入（例如 -X main.Version=...），则不要覆盖。
	if strings.TrimSpace(Version) != "" {
		return
	}

	// 默认从 embedded VERSION 文件读取版本号（编译期打包进二进制）。
	Version = strings.TrimSpace(embeddedVersion)
	if Version == "" {
		Version = "0.0.0-dev"
	}
}

// initLogger configures the default slog handler based on gin.Mode().
// In non-release mode, Debug level logs are enabled.
func main() {
	logger.InitBootstrap()
	defer logger.Sync()

	// Parse command line flags
	setupMode := flag.Bool("setup", false, "Run setup wizard in CLI mode")
	showVersion := flag.Bool("version", false, "Show version information")
	migrateOnly := flag.String("migrate-only", "", "Apply exactly these reviewed pending migration filenames (comma separated), then exit without starting Main")
	flag.Parse()

	if runSourcesDeployCommand(*setupMode || *showVersion || sourcesMigrationRequested(flag.CommandLine)) {
		return
	}
	if sourcesMigrationRequested(flag.CommandLine) {
		if *setupMode || *showVersion {
			log.Fatal("Migration-only cannot be combined with setup or version")
		}
		if err := runSourcesMigrationsOnly(*migrateOnly); err != nil {
			log.Fatalf("Migration-only failed: %v", err)
		}
		log.Println("Reviewed migrations applied; no Main process or background services were started")
		return
	}

	if *showVersion {
		log.Printf("Sub2API %s (commit: %s, built: %s)\n", Version, Commit, Date)
		return
	}
	runtime, err := processrole.Initialize()
	if err != nil {
		log.Fatalf("Invalid native process role or background ownership: %v", err)
	}
	// Ownership precedes setup too: auto-setup applies migrations and may create
	// an administrator before normal dependency construction starts.
	if !processrole.BackgroundEnabled() && (*setupMode || setup.NeedsSetup()) {
		log.Fatal("Serving requires an already initialized database and deployment configuration; setup belongs to the background owner")
	}

	// CLI setup mode
	if *setupMode {
		if err := setup.RunCLI(); err != nil {
			log.Fatalf("Setup failed: %v", err)
		}
		return
	}

	// Check if setup is needed
	if setup.NeedsSetup() {
		// Check if auto-setup is enabled (for Docker deployment)
		if setup.AutoSetupEnabled() {
			log.Println("Auto setup mode enabled...")
			if err := setup.AutoSetupFromEnv(); err != nil {
				log.Fatalf("Auto setup failed: %v", err)
			}
			// Continue to main server after auto-setup
		} else {
			log.Println("First run detected, starting setup wizard...")
			runSetupServer()
			return
		}
	}

	// Normal server mode
	runMainServer(runtime)
}

func runSetupServer() {
	r := gin.New()
	r.Use(middleware.Recovery())
	r.Use(middleware.CORS(config.CORSConfig{}))
	r.Use(middleware.SecurityHeaders(config.CSPConfig{Enabled: true, Policy: config.DefaultCSPPolicy}, nil))

	// Register setup routes
	setup.RegisterRoutes(r)

	// Serve embedded frontend if available
	if web.HasEmbeddedFrontend() {
		r.Use(web.ServeEmbeddedFrontend())
	}

	// Get server address from config.yaml or environment variables (SERVER_HOST, SERVER_PORT)
	// This allows users to run setup on a different address if needed
	addr := config.GetServerAddress()
	log.Printf("Setup wizard available at http://%s", addr)
	log.Println("Complete the setup wizard to configure Sub2API")

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	server := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		Protocols:         protocols,
	}

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Failed to start setup server: %v", err)
	}
}

func runMainServer(runtime *processrole.Runtime) {
	// The owner descriptor is deliberately released only by process exit, after
	// every native goroutine and Cleanup step is gone. Never unlock on TERM.
	cfg, err := config.LoadForBootstrap()
	if err != nil {
		fatalStartupError("load config", err)
	}
	if err := logger.Init(logger.OptionsFromConfig(cfg.Log)); err != nil {
		fatalStartupError("initialize logger", err)
	}
	if cfg.RunMode == config.RunModeSimple {
		log.Println("⚠️  WARNING: Running in SIMPLE mode - billing and quota checks are DISABLED")
	}

	buildInfo := handler.BuildInfo{
		Version:   Version,
		BuildType: BuildType,
	}

	app, err := initializeApplication(buildInfo)
	if err != nil {
		fatalStartupError("initialize application", err)
	}
	defer app.Cleanup()
	stopSettingsRefresh := app.Settings.StartSourcesSettingsRefresh()
	defer stopSettingsRefresh()
	app.Server.Handler = runtime.Wrap(app.Server.Handler)
	if app.PluginManager != nil {
		if err := app.PluginManager.Start(context.Background()); err != nil {
			log.Printf("Plugin manager started in degraded state: %v", err)
		}
	}
	if app.PromptAudit != nil {
		if err := app.PromptAudit.Start(context.Background()); err != nil {
			// Startup continues so unrelated APIs stay up. Fail-closed (unavailable)
			// applies only when a persisted blocking policy was observed; without
			// blocking intent, Prompt Audit stays ModeOff so the gateway remains
			// usable and administrators can still disable the feature (#4560).
			log.Printf("Prompt Audit started in degraded state: %v", err)
		}
	}

	// 启动服务器
	go func() {
		if err := app.Server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatalStartupError("start server", err)
		}
	}()

	log.Printf("Server started on %s", app.Server.Addr)
	runtime.Ready()

	// 等待中断信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")
	runtime.Drain()
	// A release removes this generation from routing before TERM. Keep active
	// streams alive; the operator must never escalate a slow drain to SIGKILL.
	if err := app.Server.Shutdown(context.Background()); err != nil {
		log.Printf("Server shutdown: %v", err)
	}
	// net/http does not wait for native 202 image tasks or hijacked connections.
	// Their result persistence must finish before native queues/clients close.
	if err := runtime.Wait(context.Background()); err != nil {
		log.Printf("Instance drain: %v", err)
	}

	log.Println("Server exited")
}

func fatalStartupError(stage string, err error) {
	// Runtime logging settings can suppress the standard log bridge. Startup
	// failures must reach stderr before Exit, which also skips deferred Sync.
	fmt.Fprintf(os.Stderr, "Failed to %s: %v\n", stage, err)
	os.Exit(1)
}
