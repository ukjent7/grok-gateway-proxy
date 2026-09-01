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

	"grok-gateway-proxy/internal/config"
	"grok-gateway-proxy/internal/store"
	"grok-gateway-proxy/internal/web"
)

// version is the release version surfaced to the dashboard. Override at
// build time with -ldflags "-X main.version=X.Y.Z".
var version = "2.1.0"

func main() {
	listen := flag.String("listen", "", "HTTP 监听地址（默认读取配置文件，未配置时为 127.0.0.1:8787）")
	dataDir := flag.String("data-dir", "", "配置文件与日志数据库所在目录（默认 ./data，可用 GROK_PROXY_DATA_DIR 覆盖）")
	shutdownTimeout := flag.Duration("shutdown-timeout", 30*time.Second, "优雅关闭等待在途请求的最长时间")
	logLevel := flag.String("log-level", "info", "日志级别：debug / info / warn / error")
	logRetentionDays := flag.Int("log-retention-days", 7, "日志保留天数（0 表示永久保留）；显式指定时优先于配置文件，未指定则读配置 log_retention_days（默认 7），GROK_PROXY_LOG_RETENTION_DAYS 可覆盖")
	healthInterval := flag.Duration("health-check-interval", 30*time.Second, "上游连通性探测周期；每次探测都会向该网关已配置的 Base URL 发出 /models 请求（预置 Base URL 的网关在首次启动即会外发）。0 表示完全关闭探测，控制台的上游健康灯随之保持未知")
	flag.Parse()

	if env := os.Getenv("GROK_PROXY_LISTEN"); env != "" && *listen == "" {
		*listen = env
	}
	if env := os.Getenv("GROK_PROXY_DATA_DIR"); env != "" && *dataDir == "" {
		*dataDir = env
	}
	// Retention precedence: explicit flag > env var > config file > default (7 days).
	var explicitRetention *int
	if env := os.Getenv("GROK_PROXY_LOG_RETENTION_DAYS"); env != "" {
		if days, err := strconv.Atoi(env); err == nil {
			d := days
			explicitRetention = &d
		} else {
			fmt.Fprintf(os.Stderr, "GROK_PROXY_LOG_RETENTION_DAYS invalid, ignoring: %v\n", err)
		}
	}
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "log-retention-days" {
			explicitRetention = logRetentionDays
		}
	})

	level := parseLogLevel(*logLevel)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	dataPath := *dataDir
	if dataPath == "" {
		dataPath = defaultDataDir()
	}

	config.SetProductVersion(version)

	cfg, err := config.LoadConfig(filepath.Join(dataPath, "config.json"), explicitRetention)
	if err != nil {
		logger.Error("加载配置失败", "error", err)
		os.Exit(1)
	}
	if *listen != "" {
		cfg.SetListenAddr(*listen)
	}
	listenAddr := cfg.ListenAddr()
	if !isLoopbackAddr(listenAddr) {
		logger.Warn(
			"非回环监听地址：管理 API（/api/*）无鉴权，切勿暴露到公共网络",
			"addr", listenAddr,
		)
	}
	if err := os.MkdirAll(dataPath, 0o700); err != nil {
		logger.Error("create data directory", "error", err)
		os.Exit(1)
	}
	if cfg.ShouldPersist() {
		if err := cfg.Save(); err != nil {
			logger.Error("save configuration", "error", err)
			os.Exit(1)
		}
	}

	st, err := store.OpenStore(filepath.Join(dataPath, "proxy.db"))
	if err != nil {
		logger.Error("open SQLite store", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Maintain log retention and WAL checkpoint.
	maintain := func() {
		pruned := int64(0)
		retention := cfg.GetLogRetention()
		if retention > 0 {
			var err error
			pruned, err = st.PruneOlderThan(ctx, retention)
			if err != nil {
				logger.Warn("log pruning failed", "error", err)
			} else if pruned > 0 {
				logger.Info("pruned old logs", "count", pruned)
			}
		}
		if pruned > 0 {
			if err := st.ReclaimSpace(ctx); err != nil {
				logger.Warn("reclaiming space after prune failed", "error", err)
			}
		} else if err := st.CheckpointWAL(ctx); err != nil {
			logger.Warn("WAL checkpoint failed", "error", err)
		}
	}
	maintain()

	go func() {
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

	app := web.NewApp(cfg, st, logger, version)

	if *healthInterval > 0 {
		go app.StartHealthCheck(ctx, *healthInterval)
	} else {
		logger.Info("upstream health checks disabled; /healthz reports no upstream status")
	}

	server := &http.Server{
		Addr: listenAddr,
		Handler: web.Chain(app,
			web.RecoverMiddleware(logger),
			web.SecurityHeadersMiddleware,
		),
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

	logger.Info("grok gateway proxy listening", "addr", listenAddr, "data_dir", dataPath)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("HTTP server", "error", err)
		return
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
// 空 host（例如 :8787）表示监听全部网络接口（0.0.0.0 / ::），非回环地址。
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
