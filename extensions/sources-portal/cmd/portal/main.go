package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"sources.local/portal/internal/portal"
)

const (
	checkConfigFlag = "-check-config"
	checkConfigOK   = "SOURCES_PORTAL_CONFIG_OK"
	usageLine       = "usage: sources-portal [-check-config]"
)

// Start-up failure messages. The normal start and -check-config report the
// same message for the same problem; none contains a secret or an env value.
const (
	errSubOrigin          = "SUB_ORIGIN must be a fixed HTTPS main-site origin"
	errPortalOrigin       = "PORTAL_ORIGIN must be a fixed HTTPS extension origin"
	errSameOrigin         = "Portal must use reviewed same-origin main-site routes"
	errKeyFile            = "PORTAL_KEY_FILE must be an owner-only regular file"
	errKeyRead            = "encryption key read failed"
	errKeyFormat          = "encryption key must contain 64 hex characters"
	errDataDirRequired    = "PORTAL_DATA_DIR is required"
	errDataDirMissing     = "PORTAL_DATA_DIR must be an existing directory"
	errStore              = "extension store initialization failed"
	errWebhook            = "invalid alert webhook"
	errIntelligenceStore  = "intelligence store initialization failed"
	errIntelligenceOrigin = "INTELLIGENCE_PROVIDER_ORIGIN must be a fixed HTTPS detection service origin"
	errIntelligence       = "intelligence worker initialization failed"
	errHomeDir            = "PORTAL_HOME_DIR must contain built home assets"
	errHome               = "homepage content initialization failed"
	errInternalOrigin     = "invalid fixed internal main origin"
	errBridgeKey          = "wallet bridge key must be owner-only"
	errBridgeConfig       = "invalid wallet bridge configuration"
	errBridgeOwner        = "wallet bridge requires an explicitly reviewed single notification owner"
	errTrustedProxy       = "invalid trusted proxy CIDR"
	errStudioConfigRead   = "studio config unavailable"
	errStudioConfig       = "invalid studio config"
	errStudio             = "studio bindings initialization failed"
	errStudioAPILabels    = "STUDIO_CUSTOM_API_LABELS must contain at most 16 unique lowercase DNS labels"
	errStudioURL          = "fixed HTTPS STUDIO_URL required"
	errStudioOrigin       = "STUDIO_URL must use a separate origin so the workbench cannot read Main login storage"
	errCanvasURL          = "CANVAS_URL requires a fixed HTTPS page on a separate origin"
	errListener           = "extension HTTP listener unavailable"
	errServer             = "extension HTTP server failed"
)

type startupError string

func (e startupError) Error() string { return string(e) }

// stateOpeners is the only difference between a start and -check-config while
// configuration is validated: the start takes the lock and may create or
// migrate state; the check reads and validates the same files and writes nothing.
type stateOpeners struct {
	check             bool
	store             func(string, []byte) (*portal.Store, error)
	intelligenceStore func(*portal.Store) (*portal.IntelligenceStore, error)
	intelligence      func(*portal.IntelligenceStore, string, string, string) (*portal.IntelligenceService, error)
}

var (
	startState = stateOpeners{store: portal.OpenStore, intelligenceStore: portal.OpenIntelligenceStore, intelligence: portal.NewIntelligenceServiceWithAPIOrigins}
	checkState = stateOpeners{check: true, store: portal.CheckStore, intelligenceStore: portal.CheckIntelligenceStore, intelligence: portal.CheckIntelligenceService}
)

type portalSetup struct {
	store  *portal.Store
	svc    *portal.Service
	server *portal.Server
	addr   string
}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 0:
		return serve(getenv, log.New(stderr, "", log.LstdFlags))
	case len(args) == 1 && args[0] == checkConfigFlag:
		if err := checkConfig(getenv); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, checkConfigOK)
		return 0
	default:
		fmt.Fprintln(stderr, usageLine)
		return 2
	}
}

// checkConfig runs every start-up validation without taking the state lock,
// writing state, listening or starting workers, so a deploy can verify a new
// build against the real configuration while the old process still runs.
func checkConfig(getenv func(string) string) error {
	setup, err := prepare(getenv, checkState)
	if err != nil {
		return err
	}
	defer setup.store.Close()
	// Only binding proves an address; the check verifies its form.
	if _, port, err := net.SplitHostPort(setup.addr); err != nil {
		return startupError(errListener)
	} else if _, err := net.LookupPort("tcp", port); err != nil {
		return startupError(errListener)
	}
	return nil
}

func serve(getenv func(string) string, logger *log.Logger) int {
	setup, err := prepare(getenv, startState)
	if err != nil {
		logger.Print(err)
		return 1
	}
	defer setup.store.Close()
	httpServer := &http.Server{Addr: setup.addr, Handler: setup.server.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32768}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	listener, err := net.Listen("tcp", setup.addr)
	if err != nil {
		logger.Print(errListener)
		return 1
	}
	logger.Print("Sources portal started; no main database connection")
	if err := runPortal(ctx, httpServer, listener, setup.svc.Run); err != nil {
		logger.Print(errServer)
		return 1
	}
	return 0
}

// prepare validates the configuration in start-up order and builds the
// server. On failure it releases the store it opened and returns the message.
func prepare(getenv func(string) string, open stateOpeners) (setup *portalSetup, failure error) {
	fail := func(message string) (*portalSetup, error) { return nil, startupError(message) }
	sub, err := portal.CanonicalOrigin(getenv("SUB_ORIGIN"))
	if err != nil {
		return fail(errSubOrigin)
	}
	public, err := portal.CanonicalOrigin(getenv("PORTAL_ORIGIN"))
	if err != nil {
		return fail(errPortalOrigin)
	}
	if public != sub {
		return fail(errSameOrigin)
	}
	keyPath := getenv("PORTAL_KEY_FILE")
	info, err := os.Stat(keyPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fail(errKeyFile)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return fail(errKeyRead)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != 32 {
		return fail(errKeyFormat)
	}
	dir := getenv("PORTAL_DATA_DIR")
	if dir == "" {
		return fail(errDataDirRequired)
	}
	if open.check {
		// A start creates a missing directory and would come up with empty state;
		// in a check that means a wrong mount.
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return fail(errDataDirMissing)
		}
	}
	store, err := open.store(dir, key)
	if err != nil {
		return fail(errStore)
	}
	defer func() {
		if failure != nil {
			store.Close()
		}
	}()
	client := portal.BalanceClient()
	svc := portal.NewService(store, portal.Provider{Client: client})
	svc.NotifyClient = portal.PublicClient()
	if hook := getenv("BALANCE_ALERT_WEBHOOK"); hook != "" {
		u, err := url.Parse(hook)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
			return fail(errWebhook)
		}
		svc.Webhook = hook
	}
	server := &portal.Server{Service: svc, SubOrigin: sub, PublicOrigin: public, AuthClient: portal.PublicClient()}
	intelligenceStore, err := open.intelligenceStore(store)
	if err != nil {
		return fail(errIntelligenceStore)
	}
	intelligenceProvider, err := portal.CanonicalOrigin(getenv("INTELLIGENCE_PROVIDER_ORIGIN"))
	if err != nil {
		return fail(errIntelligenceOrigin)
	}
	server.Intelligence, err = open.intelligence(intelligenceStore, sub, intelligenceProvider, getenv("INTELLIGENCE_API_ORIGIN_OVERRIDES"))
	if err != nil {
		return fail(errIntelligence)
	}
	svc.Intelligence = server.Intelligence
	if homeDir := getenv("PORTAL_HOME_DIR"); homeDir != "" {
		if info, err := os.Stat(homeDir + "/index.html"); err != nil || !info.Mode().IsRegular() {
			return fail(errHomeDir)
		}
		server.Content, err = portal.OpenContentStore(dir)
		if err != nil {
			return fail(errHome)
		}
		server.HomeFiles = os.DirFS(homeDir)
	}
	if internal := getenv("SUB_INTERNAL_ORIGIN"); internal != "" {
		u, e := url.Parse(internal)
		if e != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" {
			return fail(errInternalOrigin)
		}
		server.InternalSubOrigin = internal
		server.AuthClient = &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 8 * time.Second}
	}

	if path := getenv("WALLET_BRIDGE_KEY_FILE"); path != "" {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fail(errBridgeKey)
		}
		raw, err := os.ReadFile(path)
		token := strings.TrimSpace(string(raw))
		decoded, decodeErr := hex.DecodeString(token)
		if err != nil || decodeErr != nil || len(decoded) != 32 || server.InternalSubOrigin == "" {
			return fail(errBridgeConfig)
		}
		svc.Bridge = portal.NewWalletBridge(server.InternalSubOrigin, token)
		svc.Bridge.MainOrigin = sub
		if svc.Webhook != "" {
			return fail(errBridgeOwner)
		}
	}
	for _, raw := range strings.Split(getenv("PORTAL_TRUSTED_PROXIES"), ",") {
		if raw == "" {
			continue
		}
		p, e := netip.ParsePrefix(raw)
		if e != nil {
			return fail(errTrustedProxy)
		}
		server.TrustedProxies = append(server.TrustedProxies, p)
	}
	customAPILabels, labelErr := portal.ParseStudioCustomAPILabels(getenv("STUDIO_CUSTOM_API_LABELS"))
	if labelErr != nil {
		return fail(errStudioAPILabels)
	}
	if file := getenv("STUDIO_CONFIG_FILE"); file != "" {
		b, e := os.ReadFile(file)
		if e != nil {
			return fail(errStudioConfigRead)
		}
		var caps []portal.StudioCapability
		if json.Unmarshal(b, &caps) != nil {
			return fail(errStudioConfig)
		}
		// Same private setting as Main: the engine value an existing registry
		// and Studio client already use. Not echoed; it is private configuration.
		server.Studio, err = portal.OpenStudio(dir, sub, caps, getenv("SOURCES_MEDIA_ENGINE_LEGACY_NAME"), customAPILabels)
		if err != nil {
			return fail(errStudio)
		}
		studio, e := url.Parse(getenv("STUDIO_URL"))
		if e != nil || studio.Scheme != "https" || studio.Host == "" || studio.User != nil || studio.RawQuery != "" || studio.Fragment != "" {
			return fail(errStudioURL)
		}
		if studio.Scheme+"://"+studio.Host == sub {
			return fail(errStudioOrigin)
		}
		server.StudioURL = studio.String()
		if raw := getenv("CANVAS_URL"); raw != "" {
			canvas, e := url.Parse(raw)
			if e != nil || canvas.Scheme != "https" || canvas.Host == "" || canvas.User != nil || canvas.RawQuery != "" || canvas.Fragment != "" || canvas.Scheme+"://"+canvas.Host == sub {
				return fail(errCanvasURL)
			}
			server.CanvasURL = canvas.String()
		}
	}
	addr := getenv("PORTAL_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:18090"
	}
	return &portalSetup{store: store, svc: svc, server: server, addr: addr}, nil
}
