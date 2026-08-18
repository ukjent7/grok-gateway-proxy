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
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	listen := flag.String("listen", "", "HTTP listen address; defaults to the saved configuration")
	dataDir := flag.String("data-dir", "", "data directory for configuration and SQLite logs")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	dataPath := *dataDir
	if dataPath == "" {
		dataPath = defaultDataDir()
	}

	cfg, err := LoadConfig(filepath.Join(dataPath, "config.json"))
	if err != nil {
		logger.Error("load configuration", "error", err)
		os.Exit(1)
	}
	if *listen != "" {
		cfg.ListenAddr = *listen
	}
	if err := os.MkdirAll(dataPath, 0o700); err != nil {
		logger.Error("create data directory", "error", err)
		os.Exit(1)
	}
	if err := cfg.Save(); err != nil {
		logger.Error("save configuration", "error", err)
		os.Exit(1)
	}

	store, err := OpenStore(filepath.Join(dataPath, "proxy.db"))
	if err != nil {
		logger.Error("open SQLite store", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	app := NewApp(cfg, store, logger)
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      0,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown HTTP server", "error", err)
		}
	}()

	logger.Info("grok gateway proxy listening", "addr", cfg.ListenAddr, "data_dir", dataPath)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("HTTP server", "error", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "grok gateway proxy stopped")
}

func defaultDataDir() string {
	if appData := os.Getenv("APPDATA"); appData != "" {
		return filepath.Join(appData, "grok-gateway-proxy")
	}
	if dataHome := os.Getenv("XDG_DATA_HOME"); dataHome != "" {
		return filepath.Join(dataHome, "grok-gateway-proxy")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "grok-gateway-proxy")
	}
	return ".grok-gateway-proxy"
}
