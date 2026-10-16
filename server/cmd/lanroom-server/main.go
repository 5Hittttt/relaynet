package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"lanroom/server/internal/app"
)

func main() {
	addr := strings.TrimSpace(os.Getenv("LANROOM_ADDR"))
	if addr == "" {
		addr = ":8080"
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	statePath := strings.TrimSpace(os.Getenv("LANROOM_STATE_FILE"))
	if statePath == "" {
		statePath = "lanroom-state.json"
	}
	var vpn *app.SoftEther
	vpnConfigured := os.Getenv("LANROOM_VPNCMD") != "" || os.Getenv("LANROOM_VPN_ADMIN_PASSWORD") != "" || os.Getenv("LANROOM_VPN_PUBLIC_HOST") != ""
	chatOnly := os.Getenv("LANROOM_CHAT_ONLY") == "1"
	if vpnConfigured && chatOnly {
		logger.Error("LANROOM_CHAT_ONLY cannot be used with LANROOM_VPN* settings")
		os.Exit(1)
	}
	if !vpnConfigured && !chatOnly {
		logger.Error("VPN is not configured; set LANROOM_VPNCMD, LANROOM_VPN_ADMIN_PASSWORD and LANROOM_VPN_PUBLIC_HOST, or set LANROOM_CHAT_ONLY=1 for room/chat development")
		os.Exit(1)
	}
	if vpnConfigured {
		port := 443
		if value := os.Getenv("LANROOM_VPN_PUBLIC_PORT"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				logger.Error("invalid LANROOM_VPN_PUBLIC_PORT", "error", err)
				os.Exit(1)
			}
			port = parsed
		}
		var err error
		vpn, err = app.NewSoftEther(app.SoftEtherConfig{
			Command:       os.Getenv("LANROOM_VPNCMD"),
			AdminAddress:  os.Getenv("LANROOM_VPN_ADMIN_ADDRESS"),
			AdminPassword: os.Getenv("LANROOM_VPN_ADMIN_PASSWORD"),
			PublicHost:    os.Getenv("LANROOM_VPN_PUBLIC_HOST"),
			PublicPort:    port,
		})
		if err != nil {
			logger.Error("SoftEther configuration failed", "error", err)
			os.Exit(1)
		}
		if err := vpn.Check(context.Background()); err != nil {
			logger.Error("SoftEther VPN Server is unavailable", "error", err)
			os.Exit(1)
		}
	} else {
		logger.Warn("starting in chat-only mode; virtual networking is disabled")
	}
	application, err := app.NewWithConfig(logger, statePath, vpn)
	if err != nil {
		logger.Error("room state failed to open", "error", err)
		os.Exit(1)
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           application.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("LAN Room server listening", "address", addr)
		serverErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
			_ = server.Close()
		}
	}
}
