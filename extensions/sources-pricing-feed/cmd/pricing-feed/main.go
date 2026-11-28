package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sources.local/pricingfeed/internal/feed"
)

func main() {
	if err := run(); err != nil {
		slog.Error("pricing_feed_failed", "reason", err.Error())
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/config/config.json", "configuration file")
	dataDir := flag.String("data-dir", "/data", "persistent state directory")
	listen := flag.String("listen", ":8080", "HTTP listening address")
	check := flag.Bool("check-config", false, "validate configuration without opening state or starting a server")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	c, err := feed.ReadConfig(*configPath)
	if err != nil {
		return err
	}
	if *check {
		logger.Info("config_valid")
		return nil
	}
	service, err := feed.New(c, *dataDir, logger)
	if err != nil {
		return err
	}
	defer service.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() { defer close(done); service.Run(ctx) }()
	server := &http.Server{Addr: *listen, Handler: service.Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	errCh := make(chan error, 1)
	var serveErr error
	go func() { errCh <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = fmt.Errorf("HTTP listener failed")
		}
		stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if server.Shutdown(shutdownCtx) != nil {
		server.Close()
	}
	<-done
	return serveErr
}
