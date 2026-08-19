package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// version is the release version surfaced to the dashboard. Override at
// build time with -ldflags "-X main.version=X.Y.Z".
var version = "2.1.0"

func main() {
	listen := flag.String("listen", "", "HTTP 监听地址（默认读取配置文件，未配置时为 127.0.0.1:8787）")
	dataDir := flag.String("data-dir", "", "配置文件与日志数据库所在目录（默认 ./data，可用 GROK_PROXY_DATA_DIR 覆盖）")
	shutdownTimeout := flag.Duration("shutdown-timeout", 30*time.Second, "优雅关闭等待在途请求的最长时间")
	logLevel := flag.String("log-level", "info", "日志级别：debug / info / warn / error")
	logRetentionDays := flag.Int("log-retention-days", 30, "日志保留天数（0 表示永久保留，可用 GROK_PROXY_LOG_RETENTION_DAYS 覆盖）")
	flag.Parse()

	if env := os.Getenv("GROK_PROXY_LISTEN"); env != "" && *listen == "" {
		*listen = env
	}
	if env := os.Getenv("GROK_PROXY_DATA_DIR"); env != "" && *dataDir == "" {
		*dataDir = env
	}
	if env := os.Getenv("GROK_PROXY_LOG_RETENTION_DAYS"); env != "" {
		if days, err := strconv.Atoi(env); err == nil {
			*logRetentionDays = days
		} else {
			fmt.Fprintf(os.Stderr, "GROK_PROXY_LOG_RETENTION_DAYS invalid, ignoring: %v\n", err)
		}
	}

	level := parseLogLevel(*logLevel)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	dataPath := *dataDir
	if dataPath == "" {
		dataPath = defaultDataDir()
	}

	cfg, err := LoadConfig(filepath.Join(dataPath, "config.json"), *logRetentionDays)
	if err != nil {
		logger.Error("加载配置失败", "error", err)
		os.Exit(1)
	}
	if *listen != "" {
		cfg.ListenAddr = *listen
	}
	if !isLoopbackAddr(cfg.ListenAddr) {
		logger.Warn(
			"非回环监听地址：管理 API（/api/*）无鉴权，含改网关配置、读全部请求/响应体（含完整 prompt）、清日志等，切勿暴露到网络",
			"addr", cfg.ListenAddr,
		)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 启动时按保留策略清理过期日志，并周期性清理与 WAL checkpoint，避免数据库
	// 与 -wal 文件无限增长。
	go func() {
		maintain := func() {
			if cfg.LogRetention > 0 {
				if pruned, err := store.PruneOlderThan(ctx, cfg.LogRetention); err != nil {
					logger.Warn("log pruning failed", "error", err)
				} else if pruned > 0 {
					logger.Info("pruned old logs", "count", pruned)
				}
			}
			if err := store.CheckpointWAL(ctx); err != nil {
				logger.Warn("WAL checkpoint failed", "error", err)
			}
		}
		maintain()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				maintain()
			}
		}
	}()

	app := NewApp(cfg, store, logger)

	// 后台定期探测各启用网关的可达性，供 /healthz 报告真实上游健康状态。
	go app.startHealthCheck(ctx, 30*time.Second)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      0,
		IdleTimeout:       2 * time.Minute,
	}

	go func() {
		<-ctx.Done()
		logger.Info("shutting down", "timeout", shutdownTimeout.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Warn("graceful shutdown timed out", "error", err)
		}
	}()

	logger.Info("grok gateway proxy listening", "addr", cfg.ListenAddr, "data_dir", dataPath)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("HTTP server", "error", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "grok gateway proxy stopped")
}

func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func defaultDataDir() string {
	return filepath.Join(".", "data")
}

// isLoopbackAddr 判断监听地址是否仅回环（127.0.0.0/8、::1 或 localhost）。
// 无法解析时保守返回 true（不告警），避免误报。
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.TrimSpace(host)
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip == nil || ip.IsLoopback()
}
