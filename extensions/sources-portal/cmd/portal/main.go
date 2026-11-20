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
	"strconv"
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

const workbenchURLRequirement = " requires a fixed HTTPS page without credentials or a nonempty query or fragment, with a canonical ASCII host and unpadded port (no trailing dot)"

// Start-up failure messages. The normal start and -check-config report the
// same message for the same problem; none contains a secret or an env value.
const (
	errSubOrigin          = "SUB_ORIGIN must be a fixed HTTPS main-site origin with a canonical ASCII host and unpadded port"
	errPortalOrigin       = "PORTAL_ORIGIN must be a fixed HTTPS extension origin with a canonical ASCII host and unpadded port"
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
	errStudioConfigRead   = "STUDIO_CONFIG_FILE must be a readable regular file"
	errStudioConfig       = "invalid studio config"
	errStudio             = "studio bindings initialization failed"
	errStudioAPILabels    = "STUDIO_CUSTOM_API_LABELS must contain at most 16 unique lowercase DNS labels"
	errStudioURL          = "STUDIO_URL" + workbenchURLRequirement
	errStudioOrigin       = "STUDIO_URL must use a separate origin so the workbench cannot read Main login storage"
	errCanvasURL          = "CANVAS_URL" + workbenchURLRequirement
	errCanvasOrigin       = "CANVAS_URL must use a separate origin so the canvas cannot read Main login storage"
	errListenAddress      = "PORTAL_LISTEN requires host:port with an empty host, IP or valid hostname and a numeric port from 0 to 65535"
	errListener           = "extension HTTP listener unavailable"
	errServer             = "extension HTTP server failed"
)

type startupError string

func (e startupError) Error() string { return string(e) }

// stateOpeners selects the startup mode. A normal start binds the listener,
// then takes the lock and may create or migrate state. The check only reads
// and validates state.
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
	listener net.Listener
	store    *portal.Store
	svc      *portal.Service
	server   *portal.Server
}

func (s *portalSetup) close() {
	if s.listener != nil {
		_ = s.listener.Close()
	}
	_ = s.store.Close()
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

// checkConfig validates startup settings and readable state while the old
// process can still hold the lock. Only serve binds the listener.
func checkConfig(getenv func(string) string) error {
	setup, err := prepare(getenv, checkState)
	if err != nil {
		return err
	}
	defer setup.close()
	return nil
}

func serve(getenv func(string) string, logger *log.Logger) int {
	setup, err := prepare(getenv, startState)
	if err != nil {
		logger.Print(err)
		return 1
	}
	defer setup.close()
	httpServer := &http.Server{Handler: setup.server.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32768}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	logger.Print("Sources portal started; no main database connection")
	if err := runPortal(ctx, httpServer, setup.listener, setup.svc.Run, logger, setup.svc.Intelligence.RoundActive); err != nil {
		logger.Print(errServer)
		return 1
	}
	return 0
}

// prepare validates configuration and binds the listener before opening writable
// state on a normal start. HTTP serving only begins after initialization.
// On failure prepare closes any listener and store it opened.
func prepare(getenv func(string) string, open stateOpeners) (setup *portalSetup, failure error) {
	fail := func(message string) (*portalSetup, error) { return nil, startupError(message) }
	sub, err := canonicalMainSiteOrigin(getenv("SUB_ORIGIN"))
	if err != nil {
		return fail(errSubOrigin)
	}
	public, err := canonicalMainSiteOrigin(getenv("PORTAL_ORIGIN"))
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
	// These constructors only validate configuration or build idle clients.
	// Attach writable stores only after every environment value is validated.
	client := portal.BalanceClient()
	svc := portal.NewService(nil, portal.Provider{Client: client})
	svc.NotifyClient = portal.PublicClient()
	if hook := getenv("BALANCE_ALERT_WEBHOOK"); hook != "" {
		u, err := url.Parse(hook)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
			return fail(errWebhook)
		}
		svc.Webhook = hook
	}
	server := &portal.Server{Service: svc, SubOrigin: sub, PublicOrigin: public, AuthClient: portal.PublicClient()}
	intelligenceProvider, err := portal.CanonicalOrigin(getenv("INTELLIGENCE_PROVIDER_ORIGIN"))
	if err != nil {
		return fail(errIntelligenceOrigin)
	}
	intelligenceOverrides := getenv("INTELLIGENCE_API_ORIGIN_OVERRIDES")
	if _, err := portal.CheckIntelligenceService(nil, sub, intelligenceProvider, intelligenceOverrides); err != nil {
		return fail(errIntelligence)
	}
	homeDir := getenv("PORTAL_HOME_DIR")
	if homeDir != "" {
		if info, err := os.Stat(homeDir + "/index.html"); err != nil || !info.Mode().IsRegular() {
			return fail(errHomeDir)
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
	var caps []portal.StudioCapability
	var mediaEngineLegacyName string
	if file := getenv("STUDIO_CONFIG_FILE"); file != "" {
		b, e := readStudioConfig(file)
		if e != nil {
			return nil, e
		}
		if json.Unmarshal(b, &caps) != nil || caps == nil {
			return fail(errStudioConfig)
		}
		// Same private setting as Main: the engine value an existing registry
		// and Studio client already use. Not echoed; it is private configuration.
		mediaEngineLegacyName = getenv("SOURCES_MEDIA_ENGINE_LEGACY_NAME")
		if err := portal.CheckStudioCapabilities(caps, mediaEngineLegacyName); err != nil {
			return fail(errStudio)
		}
		server.StudioURL, err = validateWorkbenchURL(getenv("STUDIO_URL"), sub, errStudioURL, errStudioOrigin)
		if err != nil {
			return nil, err
		}
		if raw := getenv("CANVAS_URL"); raw != "" {
			server.CanvasURL, err = validateWorkbenchURL(raw, sub, errCanvasURL, errCanvasOrigin)
			if err != nil {
				return nil, err
			}
		}
	}
	addr := getenv("PORTAL_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:18090"
	}
	// Only binding proves an address; both modes validate its form before any
	// lock file, directory or migration can be written.
	host, port, err := net.SplitHostPort(addr)
	if err != nil || !validListenHost(host) {
		return fail(errListenAddress)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fail(errListenAddress)
	}
	// A normal start can create a missing directory. The read-only check
	// requires it to exist, and both modes reject an existing non-directory.
	info, err = os.Stat(dir)
	if (err == nil && !info.IsDir()) || (open.check && err != nil) {
		return fail(errDataDirMissing)
	}
	var listener net.Listener
	if !open.check {
		listener, err = net.Listen("tcp", addr)
		if err != nil {
			return fail(errListener)
		}
		defer func() {
			if failure != nil {
				_ = listener.Close()
			}
		}()
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
	svc.Store = store
	intelligenceStore, err := open.intelligenceStore(store)
	if err != nil {
		return fail(errIntelligenceStore)
	}
	server.Intelligence, err = open.intelligence(intelligenceStore, sub, intelligenceProvider, intelligenceOverrides)
	if err != nil {
		return fail(errIntelligence)
	}
	svc.Intelligence = server.Intelligence
	if homeDir != "" {
		server.Content, err = portal.OpenContentStore(dir)
		if err != nil {
			return fail(errHome)
		}
	}
	if caps != nil {
		server.Studio, err = portal.OpenStudio(dir, sub, caps, mediaEngineLegacyName, customAPILabels)
		if err != nil {
			return fail(errStudio)
		}
	}
	return &portalSetup{listener: listener, store: store, svc: svc, server: server}, nil
}

func readStudioConfig(path string) ([]byte, error) {
	// Open without waiting for a FIFO writer, then verify the opened file
	// before reading. Checking the path first would race with replacement.
	configErr := startupError(errStudioConfigRead)
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, configErr
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, configErr
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, configErr
	}
	return raw, nil
}

// The authorized client runs in an iframe of the HTTPS main site.
func validateWorkbenchURL(raw, mainOrigin string, formatError, sameOriginError startupError) (string, error) {
	page, err := url.Parse(raw)
	if err != nil || page.Scheme != "https" || page.RawQuery != "" || page.Fragment != "" {
		return "", formatError
	}
	origin, err := canonicalBrowserOrigin(page)
	if err != nil {
		return "", formatError
	}
	if origin == mainOrigin {
		return "", sameOriginError
	}
	return page.String(), nil
}

func validListenHost(host string) bool {
	if host == "" {
		return true
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	// Listen addresses may use fully qualified DNS names with a root dot.
	return validDNSHostname(strings.TrimSuffix(host, "."))
}
