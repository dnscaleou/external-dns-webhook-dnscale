package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dnscaleou/external-dns-webhook-dnscale/internal/config"
	"github.com/dnscaleou/external-dns-webhook-dnscale/internal/dnscale"
	"github.com/dnscaleou/external-dns-webhook-dnscale/internal/provider"
	"github.com/dnscaleou/external-dns-webhook-dnscale/internal/webhook"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("startup failed", "reason", err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	client, err := dnscale.New(cfg.Token, cfg.BaseURL, cfg.APITimeout)
	if err != nil {
		return errors.New("cannot initialize DNScale client")
	}
	defer client.Close()
	p, err := provider.New(client, cfg.Provider)
	if err != nil {
		return err
	}
	web := &webhook.Server{Provider: p, Client: client, Timeout: cfg.RequestTimeout}
	api := &http.Server{Handler: web.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: cfg.RequestTimeout + 5*time.Second, IdleTimeout: 60 * time.Second}
	health := &http.Server{Handler: web.HealthHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	apiListener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return errors.New("cannot bind webhook listener")
	}
	defer apiListener.Close()
	healthListener, err := net.Listen("tcp", cfg.HealthListen)
	if err != nil {
		return errors.New("cannot bind health listener")
	}
	defer healthListener.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	failures := make(chan error, 2)
	go func() { failures <- api.Serve(apiListener) }()
	go func() { failures <- health.Serve(healthListener) }()
	slog.Info("DNScale ExternalDNS webhook started", "version", version)
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-failures:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = api.Shutdown(shutdown)
	_ = health.Shutdown(shutdown)
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return errors.New("HTTP listener stopped unexpectedly")
	}
	return nil
}
