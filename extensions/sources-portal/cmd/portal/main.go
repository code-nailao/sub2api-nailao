package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
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

func main() {
	sub, err := portal.CanonicalOrigin(os.Getenv("SUB_ORIGIN"))
	if err != nil {
		log.Fatal("SUB_ORIGIN must be a fixed HTTPS main-site origin")
	}
	public, err := portal.CanonicalOrigin(os.Getenv("PORTAL_ORIGIN"))
	if err != nil {
		log.Fatal("PORTAL_ORIGIN must be a fixed HTTPS extension origin")
	}
	if public != sub {
		log.Fatal("Portal must use reviewed same-origin main-site routes")
	}
	keyPath := os.Getenv("PORTAL_KEY_FILE")
	info, err := os.Stat(keyPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		log.Fatal("PORTAL_KEY_FILE must be an owner-only regular file")
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		log.Fatal("encryption key read failed")
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != 32 {
		log.Fatal("encryption key must contain 64 hex characters")
	}
	dir := os.Getenv("PORTAL_DATA_DIR")
	if dir == "" {
		log.Fatal("PORTAL_DATA_DIR is required")
	}
	store, err := portal.OpenStore(dir, key)
	if err != nil {
		log.Fatal("extension store initialization failed")
	}
	defer store.Close()
	client := portal.BalanceClient()
	svc := portal.NewService(store, portal.Provider{Client: client})
	svc.NotifyClient = portal.PublicClient()
	if hook := os.Getenv("BALANCE_ALERT_WEBHOOK"); hook != "" {
		u, err := url.Parse(hook)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
			log.Fatal("invalid alert webhook")
		}
		svc.Webhook = hook
	}
	server := &portal.Server{Service: svc, SubOrigin: sub, PublicOrigin: public, AuthClient: portal.PublicClient()}
	intelligenceStore, err := portal.OpenIntelligenceStore(store)
	if err != nil {
		log.Fatal("intelligence store initialization failed")
	}
	intelligenceProvider, err := portal.CanonicalOrigin(os.Getenv("INTELLIGENCE_PROVIDER_ORIGIN"))
	if err != nil {
		log.Fatal("INTELLIGENCE_PROVIDER_ORIGIN must be a fixed HTTPS detection service origin")
	}
	server.Intelligence, err = portal.NewIntelligenceServiceWithAPIOrigins(intelligenceStore, sub, intelligenceProvider, os.Getenv("INTELLIGENCE_API_ORIGIN_OVERRIDES"))
	if err != nil {
		log.Fatal("intelligence worker initialization failed")
	}
	svc.Intelligence = server.Intelligence
	if homeDir := os.Getenv("PORTAL_HOME_DIR"); homeDir != "" {
		if info, err := os.Stat(homeDir + "/index.html"); err != nil || !info.Mode().IsRegular() {
			log.Fatal("PORTAL_HOME_DIR must contain built home assets")
		}
		server.Content, err = portal.OpenContentStore(dir)
		if err != nil {
			log.Fatal("homepage content initialization failed")
		}
		server.HomeFiles = os.DirFS(homeDir)
	}
	if internal := os.Getenv("SUB_INTERNAL_ORIGIN"); internal != "" {
		u, e := url.Parse(internal)
		if e != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" {
			log.Fatal("invalid fixed internal main origin")
		}
		server.InternalSubOrigin = internal
		server.AuthClient = &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 8 * time.Second}
	}

	if path := os.Getenv("WALLET_BRIDGE_KEY_FILE"); path != "" {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			log.Fatal("wallet bridge key must be owner-only")
		}
		raw, err := os.ReadFile(path)
		token := strings.TrimSpace(string(raw))
		decoded, decodeErr := hex.DecodeString(token)
		if err != nil || decodeErr != nil || len(decoded) != 32 || server.InternalSubOrigin == "" {
			log.Fatal("invalid wallet bridge configuration")
		}
		svc.Bridge = portal.NewWalletBridge(server.InternalSubOrigin, token)
		svc.Bridge.MainOrigin = sub
		if svc.Webhook != "" {
			log.Fatal("wallet bridge requires an explicitly reviewed single notification owner")
		}
	}
	for _, raw := range strings.Split(os.Getenv("PORTAL_TRUSTED_PROXIES"), ",") {
		if raw == "" {
			continue
		}
		p, e := netip.ParsePrefix(raw)
		if e != nil {
			log.Fatal("invalid trusted proxy CIDR")
		}
		server.TrustedProxies = append(server.TrustedProxies, p)
	}
	if file := os.Getenv("STUDIO_CONFIG_FILE"); file != "" {
		b, e := os.ReadFile(file)
		if e != nil {
			log.Fatal("studio config unavailable")
		}
		var caps []portal.StudioCapability
		if json.Unmarshal(b, &caps) != nil {
			log.Fatal("invalid studio config")
		}
		// Same private setting as Main: the engine value an existing registry
		// and Studio client already use. Not echoed; it is private configuration.
		server.Studio, err = portal.OpenStudio(dir, sub, caps, os.Getenv("SOURCES_MEDIA_ENGINE_LEGACY_NAME"))
		if err != nil {
			log.Fatal("studio bindings initialization failed")
		}
		studio, e := url.Parse(os.Getenv("STUDIO_URL"))
		if e != nil || studio.Scheme != "https" || studio.Host == "" || studio.User != nil || studio.RawQuery != "" || studio.Fragment != "" {
			log.Fatal("fixed HTTPS STUDIO_URL required")
		}
		if studio.Scheme+"://"+studio.Host == sub {
			log.Fatal("STUDIO_URL must use a separate origin so the workbench cannot read Main login storage")
		}
		server.StudioURL = studio.String()
		if raw := os.Getenv("CANVAS_URL"); raw != "" {
			canvas, e := url.Parse(raw)
			if e != nil || canvas.Scheme != "https" || canvas.Host == "" || canvas.User != nil || canvas.RawQuery != "" || canvas.Fragment != "" || canvas.Scheme+"://"+canvas.Host == sub {
				log.Fatal("CANVAS_URL requires a fixed HTTPS page on a separate origin")
			}
			server.CanvasURL = canvas.String()
		}
	}
	addr := os.Getenv("PORTAL_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:18090"
	}
	httpServer := &http.Server{Addr: addr, Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32768}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal("extension HTTP listener unavailable")
	}
	log.Print("Sources portal started; no main database connection")
	if err := runPortal(ctx, httpServer, listener, svc.Run); err != nil {
		log.Fatal("extension HTTP server failed")
	}
}
